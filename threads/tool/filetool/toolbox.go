package filetool

import "github.com/mackross/agentloom/threads/tool"

// ToolboxConfig configures a catalog containing the standard file tools.
type ToolboxConfig struct {
	// MutationQueue is applied to Write and ApplyPatch when their individual
	// configs do not specify a queue. When nil, DefaultMutationQueue is used.
	MutationQueue *MutationQueue
	// PathRestrictions is applied to all file tools when their individual
	// configs do not specify restrictions. When nil, paths are unrestricted.
	PathRestrictions *PathRestrictionConfig

	Read       ReadConfig
	Write      WriteConfig
	ApplyPatch ApplyPatchConfig
}

// AddTools adds the standard file tools to c and returns c for fluent catalog
// setup.
func AddTools(c *tool.Catalog, cfg ToolboxConfig) *tool.Catalog {
	if c == nil {
		c = tool.NewCatalog()
	}
	readCfg := cfg.Read
	if readCfg.PathRestrictions == nil {
		readCfg.PathRestrictions = cfg.PathRestrictions
	}
	AddRead(c, readCfg)
	writeCfg := cfg.Write
	if writeCfg.MutationQueue == nil {
		writeCfg.MutationQueue = cfg.MutationQueue
	}
	if writeCfg.PathRestrictions == nil {
		writeCfg.PathRestrictions = cfg.PathRestrictions
	}
	applyPatchCfg := cfg.ApplyPatch
	if applyPatchCfg.MutationQueue == nil {
		applyPatchCfg.MutationQueue = cfg.MutationQueue
	}
	if applyPatchCfg.PathRestrictions == nil {
		applyPatchCfg.PathRestrictions = cfg.PathRestrictions
	}
	AddWrite(c, writeCfg)
	AddApplyPatch(c, applyPatchCfg)
	return c
}

// NewCatalog returns a catalog populated with the standard file tools.
func NewCatalog(cfg ToolboxConfig) *tool.Catalog {
	return AddTools(tool.NewCatalog(), cfg)
}
