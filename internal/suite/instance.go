package suite

import "github.com/nesolva/localci/internal/shard"

// instance names this localci (its data directory) in the databases it makes on a shared server.
func (ex *execution) instance() string { return shard.InstanceID(ex.r.d.DataDir) }
