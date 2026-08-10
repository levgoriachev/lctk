package semantic

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/lev-goryachev/lctk/images/code-intel/internal/symbols"
)

const (
	defaultGraphLimit = 50
	maximumGraphLimit = 200
	maximumPathDepth  = 32
	// goModuleMetaKey stores the JSON {module directory: module path} mapping
	// discovered from every go.mod in the indexed workspace, published together
	// with the graph generation it describes.
	goModuleMetaKey = "graph_go_modules"
)

// GraphStatus identifies the one fully published derived graph generation.
type GraphStatus struct {
	Ready       bool   `json:"ready"`
	Generation  uint64 `json:"generation"`
	FileCount   int    `json:"file_count"`
	NodeCount   int    `json:"node_count"`
	ImportCount int    `json:"import_count"`
	CallCount   int    `json:"call_count"`
	IndexedAt   string `json:"indexed_at,omitempty"`
	Precision   string `json:"precision"`
	Reason      string `json:"reason,omitempty"`
}

// GraphRequest is the common bounded request for callers and callees.
type GraphRequest struct {
	Name   string `json:"name"`
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

// GraphEvidence is one name-matched call with its saved source coordinate.
type GraphEvidence struct {
	Path   string `json:"path"`
	Caller string `json:"caller,omitempty"`
	Callee string `json:"callee"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

// GraphMatches reports bounded call evidence and declaration ambiguity.
type GraphMatches struct {
	Name         string          `json:"name"`
	Direction    string          `json:"direction"`
	Matches      []GraphEvidence `json:"matches"`
	Total        int             `json:"total"`
	Truncated    bool            `json:"truncated"`
	NextCursor   string          `json:"next_cursor,omitempty"`
	Ambiguous    bool            `json:"ambiguous"`
	Declarations int             `json:"declarations"`
	Generation   uint64          `json:"generation"`
	Precision    string          `json:"precision"`
}

// DependencyRequest asks for a bounded file-to-file import route.
type DependencyRequest struct {
	From     string `json:"from"`
	To       string `json:"to"`
	MaxDepth int    `json:"max_depth,omitempty"`
}

// DependencyResponse is one deterministic shortest path through syntax imports.
type DependencyResponse struct {
	From       string   `json:"from"`
	To         string   `json:"to"`
	Path       []string `json:"path"`
	Found      bool     `json:"found"`
	MaxDepth   int      `json:"max_depth"`
	Generation uint64   `json:"generation"`
	Precision  string   `json:"precision"`
}

// ImpactRequest asks for reverse import and call evidence reachable from a file
// path or declaration name.
type ImpactRequest struct {
	Target string `json:"target"`
	Limit  int    `json:"limit,omitempty"`
}

// ImpactResponse keeps the two evidence kinds separate so a client never reads
// a file dependency as a resolved symbol call.
type ImpactResponse struct {
	Target     string          `json:"target"`
	Files      []string        `json:"files"`
	Calls      []GraphEvidence `json:"calls"`
	Total      int             `json:"total"`
	Truncated  bool            `json:"truncated"`
	Ambiguous  bool            `json:"ambiguous"`
	Generation uint64          `json:"generation"`
	Precision  string          `json:"precision"`
}

// MapRequest bounds repository_map by characters, the budget agents actually
// spend, instead of by an arbitrary number of nodes.
type MapRequest struct {
	MaxChars int `json:"max_chars,omitempty"`
}

// MapResponse is a deterministic compact map with explicit truncation.
type MapResponse struct {
	Map        string `json:"map"`
	Characters int    `json:"characters"`
	MaxChars   int    `json:"max_chars"`
	Truncated  bool   `json:"truncated"`
	FileCount  int    `json:"file_count"`
	NodeCount  int    `json:"node_count"`
	Generation uint64 `json:"generation"`
	Precision  string `json:"precision"`
}

func (s *Store) graphFileDigests(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT path, digest FROM graph_files")
	if err != nil {
		return nil, fail(CodeCorrupt, "Graph file state could not be read.", false, err)
	}
	defer rows.Close()
	values := map[string]string{}
	for rows.Next() {
		var name, digest string
		if err := rows.Scan(&name, &digest); err != nil {
			return nil, fail(CodeCorrupt, "Graph file state is invalid.", false, err)
		}
		values[name] = digest
	}
	return values, rows.Err()
}

func deleteGraphPath(ctx context.Context, tx *sql.Tx, name string) error {
	statements := []string{
		"DELETE FROM graph_calls WHERE path = ?",
		"DELETE FROM graph_imports WHERE source_path = ?",
		"DELETE FROM graph_nodes WHERE path = ?",
		"DELETE FROM graph_files WHERE path = ?",
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement, name); err != nil {
			return fail(CodeInternalError, "Old graph facts could not be removed.", false, err)
		}
	}
	return nil
}

func replaceGraphPath(ctx context.Context, tx *sql.Tx, facts symbols.GraphFacts) error {
	if err := deleteGraphPath(ctx, tx, facts.Path); err != nil {
		return err
	}
	for _, declaration := range facts.Declarations {
		if _, err := tx.ExecContext(ctx, `INSERT INTO graph_nodes(
path,name,kind,container,line,column_number,start_byte,end_byte,signature) VALUES(?,?,?,?,?,?,?,?,?)`,
			facts.Path, declaration.Name, declaration.Kind, declaration.Container,
			declaration.Line, declaration.Column, declaration.StartByte, declaration.EndByte,
			declaration.Signature); err != nil {
			return fail(CodeInternalError, "A graph declaration could not be published.", false, err)
		}
	}
	for _, imported := range facts.Imports {
		if _, err := tx.ExecContext(ctx, `INSERT INTO graph_imports(
source_path,target,line,column_number) VALUES(?,?,?,?)`, facts.Path, imported.Target,
			imported.Line, imported.Column); err != nil {
			return fail(CodeInternalError, "A graph import could not be published.", false, err)
		}
	}
	for _, call := range facts.Calls {
		if _, err := tx.ExecContext(ctx, `INSERT INTO graph_calls(
path,caller,callee,line,column_number) VALUES(?,?,?,?,?)`, facts.Path, call.Caller,
			call.Callee, call.Line, call.Column); err != nil {
			return fail(CodeInternalError, "A graph call could not be published.", false, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO graph_files(path,digest,language)
VALUES(?,?,?)`, facts.Path, facts.Digest, facts.Language); err != nil {
		return fail(CodeInternalError, "Graph file metadata could not be published.", false, err)
	}
	return nil
}

// GraphStatus reads the graph's committed publication boundary.
func (s *Store) GraphStatus() (GraphStatus, error) {
	status := GraphStatus{Precision: "name_match"}
	var generation string
	err := s.db.QueryRow("SELECT value FROM semantic_meta WHERE key='graph_generation'").Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		status.Reason = "The derived graph has not been built yet."
		return status, nil
	}
	if err != nil {
		return GraphStatus{}, fail(CodeCorrupt, "Graph publication metadata could not be read.", false, err)
	}
	value, err := strconv.ParseUint(generation, 10, 64)
	if err != nil {
		return GraphStatus{}, fail(CodeCorrupt, "The graph generation is invalid.", false, err)
	}
	status.Generation = value
	if err := s.db.QueryRow("SELECT value FROM semantic_meta WHERE key='graph_indexed_at'").Scan(&status.IndexedAt); err != nil {
		return GraphStatus{}, fail(CodeCorrupt, "The graph publication time could not be read.", false, err)
	}
	counts := []struct {
		query string
		value *int
	}{{"SELECT COUNT(*) FROM graph_files", &status.FileCount}, {"SELECT COUNT(*) FROM graph_nodes", &status.NodeCount},
		{"SELECT COUNT(*) FROM graph_imports", &status.ImportCount}, {"SELECT COUNT(*) FROM graph_calls", &status.CallCount}}
	for _, count := range counts {
		if err := s.db.QueryRow(count.query).Scan(count.value); err != nil {
			return GraphStatus{}, fail(CodeCorrupt, "Graph counts could not be read.", false, err)
		}
	}
	status.Ready = true
	return status, nil
}

// Callers and Callees expose the same stored call evidence in opposite directions.
func (s *Store) Callers(ctx context.Context, request GraphRequest) (GraphMatches, error) {
	return s.callMatches(ctx, request, "callers")
}

func (s *Store) Callees(ctx context.Context, request GraphRequest) (GraphMatches, error) {
	return s.callMatches(ctx, request, "callees")
}

func (s *Store) callMatches(ctx context.Context, request GraphRequest, direction string) (GraphMatches, error) {
	name := strings.TrimSpace(request.Name)
	if name == "" || len(name) > 512 {
		return GraphMatches{}, fail(CodeInvalidQuery, "The graph name must contain between 1 and 512 bytes.", false, nil)
	}
	limit, offset, err := graphBounds(request.Limit, request.Cursor)
	if err != nil {
		return GraphMatches{}, err
	}
	status, err := s.GraphStatus()
	if err != nil || !status.Ready {
		if err != nil {
			return GraphMatches{}, err
		}
		return GraphMatches{}, fail(CodeNotReady, status.Reason, true, nil)
	}
	field := "callee"
	if direction == "callees" {
		field = "caller"
	}
	var total, declarations int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM graph_calls WHERE "+field+" = ?", name).Scan(&total); err != nil {
		return GraphMatches{}, fail(CodeCorrupt, "Graph call matches could not be counted.", false, err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM graph_nodes WHERE name = ?", name).Scan(&declarations); err != nil {
		return GraphMatches{}, fail(CodeCorrupt, "Graph declaration ambiguity could not be read.", false, err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT path,caller,callee,line,column_number FROM graph_calls WHERE `+
		field+` = ? ORDER BY path,line,column_number,caller,callee LIMIT ? OFFSET ?`, name, limit, offset)
	if err != nil {
		return GraphMatches{}, fail(CodeCorrupt, "Graph call matches could not be read.", false, err)
	}
	defer rows.Close()
	result := GraphMatches{Name: name, Direction: direction, Matches: []GraphEvidence{}, Total: total,
		Ambiguous: declarations > 1, Declarations: declarations, Generation: status.Generation, Precision: "name_match"}
	for rows.Next() {
		var evidence GraphEvidence
		if err := rows.Scan(&evidence.Path, &evidence.Caller, &evidence.Callee, &evidence.Line, &evidence.Column); err != nil {
			return GraphMatches{}, fail(CodeCorrupt, "A graph call match is invalid.", false, err)
		}
		result.Matches = append(result.Matches, evidence)
	}
	result.Truncated = offset+len(result.Matches) < total
	if result.Truncated {
		result.NextCursor = encodeGraphCursor(offset + len(result.Matches))
	}
	return result, rows.Err()
}

func graphBounds(limit int, cursor string) (int, int, error) {
	if limit == 0 {
		limit = defaultGraphLimit
	}
	if limit < 1 || limit > maximumGraphLimit {
		return 0, 0, fail(CodeInvalidQuery, "The graph result limit must be between 1 and 200.", false, nil)
	}
	if cursor == "" {
		return limit, 0, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, 0, fail(CodeInvalidQuery, "The graph cursor is invalid.", false, err)
	}
	offset, err := strconv.Atoi(string(decoded))
	if err != nil || offset < 0 {
		return 0, 0, fail(CodeInvalidQuery, "The graph cursor is invalid.", false, err)
	}
	return limit, offset, nil
}

func encodeGraphCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

type importRow struct {
	source, target string
}

func (s *Store) dependencyGraph(ctx context.Context) (map[string][]string, []string, error) {
	fileRows, err := s.db.QueryContext(ctx, "SELECT path, language FROM graph_files ORDER BY path")
	if err != nil {
		return nil, nil, fail(CodeCorrupt, "Graph files could not be read.", false, err)
	}
	var files []string
	languages := map[string]string{}
	for fileRows.Next() {
		var name, language string
		if err := fileRows.Scan(&name, &language); err != nil {
			fileRows.Close()
			return nil, nil, err
		}
		files = append(files, name)
		languages[name] = language
	}
	if err := fileRows.Close(); err != nil {
		return nil, nil, err
	}
	resolver := newImportResolver(files, s.goModuleMapping(ctx))
	rows, err := s.db.QueryContext(ctx, "SELECT source_path,target FROM graph_imports ORDER BY source_path,target")
	if err != nil {
		return nil, nil, fail(CodeCorrupt, "Graph imports could not be read.", false, err)
	}
	defer rows.Close()
	graph := map[string][]string{}
	for rows.Next() {
		var row importRow
		if err := rows.Scan(&row.source, &row.target); err != nil {
			return nil, nil, err
		}
		for _, resolved := range resolver.resolve(row.source, languages[row.source], row.target) {
			graph[row.source] = appendUnique(graph[row.source], resolved)
		}
	}
	for source := range graph {
		sort.Strings(graph[source])
	}
	return graph, files, rows.Err()
}

// goModuleMapping loads the published {module directory: module path} mapping.
// A database without the key (older generation) or with an unreadable value
// yields no module edges rather than an error, matching the graph's promise of
// evidence over invention.
func (s *Store) goModuleMapping(ctx context.Context) map[string]string {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM semantic_meta WHERE key=?", goModuleMetaKey).Scan(&raw)
	if err != nil {
		return nil
	}
	modules := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &modules); err != nil {
		return nil
	}
	return modules
}

// discoverGoModules maps each directory that holds a parseable go.mod to its
// declared module path. A manifest that cannot be read or parsed contributes
// no mapping — and therefore no edges — instead of failing the sync.
func discoverGoModules(files map[string]string, source Source, maxBytes int64) map[string]string {
	var manifests []string
	for name := range files {
		if path.Base(name) == "go.mod" {
			manifests = append(manifests, name)
		}
	}
	sort.Strings(manifests)
	modules := map[string]string{}
	for _, manifest := range manifests {
		content, _, err := source.ReadProjectFile(manifest, maxBytes)
		if err != nil {
			continue
		}
		if modulePath := goModulePath(content); modulePath != "" {
			modules[path.Dir(manifest)] = modulePath
		}
	}
	return modules
}

// goModulePath hand-parses only the `module` directive, so the store does not
// grow a go.mod parsing dependency for one line of one file.
func goModulePath(content []byte) string {
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		rest, found := strings.CutPrefix(line, "module")
		if !found || rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
			continue
		}
		value := strings.TrimSpace(rest)
		if index := strings.Index(value, "//"); index >= 0 {
			value = strings.TrimSpace(value[:index])
		}
		value = strings.Trim(value, "\"`")
		if value == "" || strings.ContainsAny(value, " \t") {
			return ""
		}
		return value
	}
	return ""
}

// goModule is one declared module rooted at a workspace directory ("." for the
// workspace root). Its path keeps any /vN major-version suffix exactly as
// go.mod declares it, because that suffix is part of every import path.
type goModule struct {
	dir  string
	path string
}

// importResolver answers import targets against one immutable file snapshot.
// The exact-path and per-directory indexes are built once per dependencyGraph
// call, replacing the per-import linear scans over every indexed file.
type importResolver struct {
	fileSet     map[string]bool
	filesByDir  map[string][]string
	directories []string
	modules     []goModule
}

func newImportResolver(files []string, modules map[string]string) *importResolver {
	resolver := &importResolver{fileSet: make(map[string]bool, len(files)), filesByDir: map[string][]string{}}
	// files arrive sorted by path, so every filesByDir slice is sorted too.
	for _, file := range files {
		resolver.fileSet[file] = true
		directory := path.Dir(file)
		resolver.filesByDir[directory] = append(resolver.filesByDir[directory], file)
	}
	resolver.directories = make([]string, 0, len(resolver.filesByDir))
	for directory := range resolver.filesByDir {
		resolver.directories = append(resolver.directories, directory)
	}
	sort.Strings(resolver.directories)
	for directory, modulePath := range modules {
		resolver.modules = append(resolver.modules, goModule{dir: directory, path: modulePath})
	}
	// The longest module path is matched first, so a nested module claims
	// imports inside its own namespace before the workspace root module can.
	sort.Slice(resolver.modules, func(i, j int) bool {
		if len(resolver.modules[i].path) != len(resolver.modules[j].path) {
			return len(resolver.modules[i].path) > len(resolver.modules[j].path)
		}
		return resolver.modules[i].path < resolver.modules[j].path
	})
	return resolver
}

// resolve returns every workspace file an import target names.
//
// Go sources resolve exclusively through declared module paths: a Go import
// names a module package or the standard library, never a sibling file, so any
// other answer would manufacture an edge the compiler does not create. The old
// base-name fallback (any file sharing the target's base name anywhere in the
// tree) is gone for every language for the same reason.
func (r *importResolver) resolve(source, language, target string) []string {
	target = strings.TrimSpace(strings.ReplaceAll(target, "\\", "/"))
	clean := strings.TrimPrefix(path.Clean(target), "./")
	if language == symbols.LanguageGo {
		return r.resolveModuleImport(source, clean)
	}
	// Module resolution is attempted for other languages only when the target is
	// shaped like a module path; relative and bare targets keep their order.
	if !strings.HasPrefix(target, ".") && r.looksLikeModulePath(clean) {
		if resolved := r.resolveModuleImport(source, clean); len(resolved) > 0 {
			return resolved
		}
	}
	base := path.Dir(source)
	python := strings.ReplaceAll(strings.TrimLeft(clean, "."), ".", "/")
	var stems []string
	if strings.HasPrefix(target, ".") {
		stems = append(stems, path.Clean(path.Join(base, clean)))
	}
	// A bare module or header is first resolved beside the importing file. Package
	// style absolute targets are tried afterwards, so local syntax is deterministic
	// without claiming package-manager knowledge.
	stems = append(stems, path.Clean(path.Join(base, clean)), clean, python)
	extensions := []string{"", ".go", ".py", ".rs", ".c", ".h", ".cc", ".cpp", ".js", ".jsx", ".ts", ".tsx"}
	for _, stem := range stems {
		for _, extension := range extensions {
			if candidate := stem + extension; r.fileSet[candidate] {
				return []string{candidate}
			}
		}
		for _, suffix := range []string{"/index.js", "/index.ts", "/__init__.py", "/mod.rs"} {
			if candidate := stem + suffix; r.fileSet[candidate] {
				return []string{candidate}
			}
		}
	}
	for _, directory := range r.directories {
		if directory == clean || strings.HasSuffix(directory, "/"+clean) {
			return []string{r.filesByDir[directory][0]}
		}
	}
	return nil
}

// resolveModuleImport maps a module-path import onto the directory owned by
// the longest matching declared module, then fans out to every Go file there,
// because a Go import names a package, not a file. `replace` directives and
// vendored dependencies are deliberately out of scope: both rewrite what a
// module path means, and honoring them requires go.mod semantics this derived
// name-match graph does not promise.
func (r *importResolver) resolveModuleImport(source, target string) []string {
	for _, module := range r.modules {
		remainder, matched := strings.CutPrefix(target, module.path)
		if !matched || (remainder != "" && !strings.HasPrefix(remainder, "/")) {
			continue
		}
		directory := path.Join(module.dir, strings.TrimPrefix(remainder, "/"))
		var resolved []string
		for _, file := range r.filesByDir[directory] {
			if strings.HasSuffix(file, ".go") && file != source {
				resolved = append(resolved, file)
			}
		}
		return resolved
	}
	return nil
}

// looksLikeModulePath recognizes module-path-shaped targets: a dotted host in
// the first path element ("example.com/pkg"), or a declared module prefix.
func (r *importResolver) looksLikeModulePath(clean string) bool {
	if first, _, found := strings.Cut(clean, "/"); found && strings.Contains(first, ".") {
		return true
	}
	for _, module := range r.modules {
		if clean == module.path || strings.HasPrefix(clean, module.path+"/") {
			return true
		}
	}
	return false
}

func appendUnique(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

// DependencyPath performs bounded breadth-first traversal and returns the first
// deterministic shortest route.
func (s *Store) DependencyPath(ctx context.Context, request DependencyRequest) (DependencyResponse, error) {
	from, to := strings.TrimSpace(request.From), strings.TrimSpace(request.To)
	if from == "" || to == "" {
		return DependencyResponse{}, fail(CodeInvalidQuery, "Both dependency path endpoints are required.", false, nil)
	}
	depth := request.MaxDepth
	if depth == 0 {
		depth = maximumPathDepth
	}
	if depth < 1 || depth > maximumPathDepth {
		return DependencyResponse{}, fail(CodeInvalidQuery, "Dependency max_depth must be between 1 and 32.", false, nil)
	}
	status, err := s.GraphStatus()
	if err != nil || !status.Ready {
		if err != nil {
			return DependencyResponse{}, err
		}
		return DependencyResponse{}, fail(CodeNotReady, status.Reason, true, nil)
	}
	graph, files, err := s.dependencyGraph(ctx)
	if err != nil {
		return DependencyResponse{}, err
	}
	from = resolveEndpoint(from, files)
	to = resolveEndpoint(to, files)
	result := DependencyResponse{From: from, To: to, Path: []string{}, MaxDepth: depth,
		Generation: status.Generation, Precision: "name_match"}
	if from == "" || to == "" {
		return result, nil
	}
	type route struct {
		node string
		path []string
	}
	queue := []route{{from, []string{from}}}
	seen := map[string]bool{from: true}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.node == to {
			result.Found, result.Path = true, current.path
			return result, nil
		}
		if len(current.path)-1 >= depth {
			continue
		}
		for _, next := range graph[current.node] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, route{next, append(append([]string{}, current.path...), next)})
			}
		}
	}
	return result, nil
}

func resolveEndpoint(value string, files []string) string {
	value = strings.TrimPrefix(path.Clean(strings.ReplaceAll(value, "\\", "/")), "./")
	for _, file := range files {
		if file == value {
			return file
		}
	}
	var matches []string
	for _, file := range files {
		if strings.HasSuffix(file, "/"+value) || path.Base(file) == value {
			matches = append(matches, file)
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

// Impact returns direct reverse dependency and name-call evidence. It does not
// invent transitive type impact that name matching cannot justify.
func (s *Store) Impact(ctx context.Context, request ImpactRequest) (ImpactResponse, error) {
	target := strings.TrimSpace(request.Target)
	limit, _, err := graphBounds(request.Limit, "")
	if err != nil {
		return ImpactResponse{}, err
	}
	if target == "" {
		return ImpactResponse{}, fail(CodeInvalidQuery, "The impact target is required.", false, nil)
	}
	status, err := s.GraphStatus()
	if err != nil || !status.Ready {
		if err != nil {
			return ImpactResponse{}, err
		}
		return ImpactResponse{}, fail(CodeNotReady, status.Reason, true, nil)
	}
	graph, files, err := s.dependencyGraph(ctx)
	if err != nil {
		return ImpactResponse{}, err
	}
	resolved := resolveEndpoint(target, files)
	fileSet := map[string]bool{}
	if resolved != "" {
		for source, destinations := range graph {
			for _, destination := range destinations {
				if destination == resolved {
					fileSet[source] = true
				}
			}
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT path,caller,callee,line,column_number
FROM graph_calls WHERE callee=? ORDER BY path,line,column_number`, target)
	if err != nil {
		return ImpactResponse{}, fail(CodeCorrupt, "Impact calls could not be read.", false, err)
	}
	defer rows.Close()
	var calls []GraphEvidence
	for rows.Next() {
		var evidence GraphEvidence
		if err := rows.Scan(&evidence.Path, &evidence.Caller, &evidence.Callee, &evidence.Line, &evidence.Column); err != nil {
			return ImpactResponse{}, err
		}
		calls = append(calls, evidence)
		fileSet[evidence.Path] = true
	}
	var impacted []string
	for file := range fileSet {
		impacted = append(impacted, file)
	}
	sort.Strings(impacted)
	total := len(impacted) + len(calls)
	if len(impacted) > limit {
		impacted = impacted[:limit]
	}
	remaining := limit - len(impacted)
	if remaining < len(calls) {
		calls = calls[:max(remaining, 0)]
	}
	var declarations int
	_ = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM graph_nodes WHERE name=?", target).Scan(&declarations)
	return ImpactResponse{Target: target, Files: impacted, Calls: calls, Total: total,
		Truncated: len(impacted)+len(calls) < total, Ambiguous: declarations > 1,
		Generation: status.Generation, Precision: "name_match"}, rows.Err()
}

// RepositoryMap ranks files by incoming/outgoing dependencies and calls, then
// adds declarations in stable order until the exact character budget is spent.
func (s *Store) RepositoryMap(ctx context.Context, request MapRequest) (MapResponse, error) {
	budget := request.MaxChars
	if budget == 0 {
		budget = 12000
	}
	if budget < 256 || budget > 100000 {
		return MapResponse{}, fail(CodeInvalidQuery, "max_chars must be between 256 and 100000.", false, nil)
	}
	status, err := s.GraphStatus()
	if err != nil || !status.Ready {
		if err != nil {
			return MapResponse{}, err
		}
		return MapResponse{}, fail(CodeNotReady, status.Reason, true, nil)
	}
	graph, files, err := s.dependencyGraph(ctx)
	if err != nil {
		return MapResponse{}, err
	}
	scores := map[string]int{}
	for source, targets := range graph {
		scores[source] += len(targets)
		for _, target := range targets {
			scores[target] += 2
		}
	}
	callRows, err := s.db.QueryContext(ctx, "SELECT path,COUNT(*) FROM graph_calls GROUP BY path")
	if err != nil {
		return MapResponse{}, err
	}
	for callRows.Next() {
		var file string
		var count int
		if err := callRows.Scan(&file, &count); err != nil {
			callRows.Close()
			return MapResponse{}, err
		}
		scores[file] += count
	}
	if err := callRows.Close(); err != nil {
		return MapResponse{}, err
	}
	sort.Slice(files, func(i, j int) bool {
		if scores[files[i]] != scores[files[j]] {
			return scores[files[i]] > scores[files[j]]
		}
		return files[i] < files[j]
	})
	var builder strings.Builder
	characters := 0
	truncated := false
	for _, file := range files {
		header := file + "\n"
		if !appendMapText(&builder, &characters, budget, header) {
			truncated = true
			continue
		}
		rows, err := s.db.QueryContext(ctx, `SELECT name,kind,container,signature FROM graph_nodes
WHERE path=? ORDER BY start_byte,name`, file)
		if err != nil {
			return MapResponse{}, err
		}
		for rows.Next() {
			var name, kind, container, signature string
			if err := rows.Scan(&name, &kind, &container, &signature); err != nil {
				rows.Close()
				return MapResponse{}, err
			}
			label := "  " + string(kind) + " " + name
			if container != "" {
				label += " in " + container
			}
			if signature != "" {
				label += " — " + signature
			}
			if !appendMapText(&builder, &characters, budget, label+"\n") {
				truncated = true
			}
		}
		if err := rows.Close(); err != nil {
			return MapResponse{}, err
		}
	}
	return MapResponse{Map: builder.String(), Characters: characters, MaxChars: budget,
		Truncated: truncated, FileCount: status.FileCount, NodeCount: status.NodeCount,
		Generation: status.Generation, Precision: "name_match"}, nil
}

// appendMapText enforces a Unicode-character budget rather than a byte budget.
// Agents spend text characters, and splitting a UTF-8 rune would also produce an
// invalid MCP result. A line that does not fit is omitted and marks truncation.
func appendMapText(builder *strings.Builder, used *int, budget int, value string) bool {
	length := utf8.RuneCountInString(value)
	if *used+length > budget {
		return false
	}
	builder.WriteString(value)
	*used += length
	return true
}
