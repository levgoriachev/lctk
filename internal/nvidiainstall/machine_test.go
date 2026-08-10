package nvidiainstall

import "testing"

func TestRemoteCommandPreservesEveryArgumentForPodmanSSH(t *testing.T) {
	got, err := remoteCommand([]string{"sh", "-ceu", "if test 'quoted value'; then\nprintf '%s\\n' safe\nfi"})
	if err != nil {
		t.Fatal(err)
	}
	want := `'sh' '-ceu' 'if test '"'"'quoted value'"'"'; then
printf '"'"'%s\n'"'"' safe
fi'`
	if got != want {
		t.Fatalf("remote command = %q, want %q", got, want)
	}
}

func TestRemoteCommandRejectsEmptyInvocation(t *testing.T) {
	if _, err := remoteCommand(nil); err == nil {
		t.Fatal("empty remote command was accepted")
	}
}
