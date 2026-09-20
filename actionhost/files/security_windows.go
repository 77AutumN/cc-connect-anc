//go:build windows

package files

import "os"

// This host needs POSIX ownership and no-follow descriptors. Windows can run
// format/schema tests, but must not quietly claim the production boundary.
func protectedRoot(string) (*os.Root, error)  { return nil, ErrUnavailable }
func checkOwner(os.FileInfo, int, bool) error { return ErrUnavailable }
func identity(os.FileInfo) string             { return "" }
func unchanged(os.FileInfo, os.FileInfo) bool { return false }
func noFollowFlag() int                       { return 0 }
func fileGroup(os.FileInfo) int               { return -1 }
