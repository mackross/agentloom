package filetool

func configString(override *string, def string) string {
	if override == nil {
		return def
	}
	return *override
}
