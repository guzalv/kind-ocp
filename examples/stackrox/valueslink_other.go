//go:build !unix

package main

import "os"

// Link counts have no portable representation on this platform, so the
// multiply-linked values refusal is skipped there; the example's
// supported runtimes (macOS, Linux, where kind and Docker run) are all
// unix, where it is enforced.

func checkValuesLinkCount(string, os.FileInfo) error { return nil }
