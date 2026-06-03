package filetool

import (
	"context"

	"github.com/mackross/agentloom/threads/tool/filetool/fileprocess"
	"github.com/mackross/agentloom/threads/tool/filetool/golangprocessor"
)

type FileOperation = fileprocess.Operation

const (
	FileOperationWrite       = fileprocess.OperationWrite
	FileOperationEdit        = fileprocess.OperationEdit
	FileOperationPatchAdd    = fileprocess.OperationPatchAdd
	FileOperationPatchUpdate = fileprocess.OperationPatchUpdate
	FileOperationPatchMove   = fileprocess.OperationPatchMove
)

type FilePostprocessRequest = fileprocess.Request
type FilePostprocessResult = fileprocess.Result
type FilePostprocessReport = fileprocess.Report
type FilePostprocessor = fileprocess.Processor
type FilePostprocessorFunc = fileprocess.ProcessorFunc
type PostprocessConfig = fileprocess.Config

// DefaultPostprocessors returns the built-in processors used by zero-value file
// tool configs. The returned slice is fresh and may be safely modified by the
// caller.
func DefaultPostprocessors() []FilePostprocessor {
	return []FilePostprocessor{
		golangprocessor.Default(),
	}
}

func runPostprocess(ctx context.Context, cfg PostprocessConfig, req FilePostprocessRequest) (FilePostprocessResult, []FilePostprocessReport, error) {
	return fileprocess.Run(ctx, req, fileprocess.Pipeline(cfg, DefaultPostprocessors()))
}
