package core

type Item interface {
	Emit() bool
	MergesWith() []any
}
