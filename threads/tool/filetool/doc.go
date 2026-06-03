// Package filetool provides AgentLoom-native tools for reading and mutating
// files.
//
// The package is intentionally small at its public boundary. Tools are composed
// into applications with threads/tool.Catalog, while shared file concerns such
// as path resolution and output truncation stay private until callers need a
// broader policy API.
package filetool
