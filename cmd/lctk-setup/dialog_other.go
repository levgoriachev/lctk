//go:build !windows

package main

import (
	"fmt"

	"github.com/lev-goryachev/lctk/internal/lctkhome"
)

func showError(message string)                         { fmt.Println("LCTK Setup:", message) }
func showInfo(message string)                          { fmt.Println("LCTK Setup:", message) }
func confirmUninstall(lctkhome.Locations) (bool, bool) { return false, false }
