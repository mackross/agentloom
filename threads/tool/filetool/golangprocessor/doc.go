// Package golangprocessor provides the default Go file processor for filetool.
//
// The processor uses golang.org/x/tools/imports to format Go source and keep
// imports in sync. It implements the lower-level fileprocess.Processor protocol
// so it can be used by write, apply_patch, and future content-producing tools.
package golangprocessor
