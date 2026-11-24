package config

import "flag"

// FlagValues records which config flags the user actually set.
type FlagValues struct {
	fs *flag.FlagSet
}

// RegisterFlags adds one --section-name flag per overridable key.
func RegisterFlags(fs *flag.FlagSet) *FlagValues {
	for i := range Fields {
		f := &Fields[i]
		if f.noEnv {
			continue
		}
		usage := f.Doc
		fs.String(FlagName(f.Key), "", usage)
	}
	return &FlagValues{fs: fs}
}

// Values returns key -> raw value for the flags that were set on the command line.
func (v *FlagValues) Values() map[string]string {
	out := map[string]string{}
	v.fs.Visit(func(fl *flag.Flag) {
		for i := range Fields {
			if !Fields[i].noEnv && FlagName(Fields[i].Key) == fl.Name {
				out[Fields[i].Key] = fl.Value.String()
			}
		}
	})
	return out
}
