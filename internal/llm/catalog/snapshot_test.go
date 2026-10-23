package catalog

import "testing"

func TestReadOnlySnapshotIsDetachedAndNeverRefreshes(t *testing.T) {
	client := NewStaticClient(Catalog{"route": {ID: "route", Env: []string{"KEY"}, Models: map[string]Model{"m": {ID: "m", Limit: Limit{Context: 128000}, Modalities: Modalities{Input: []string{"text", "image"}}}}}})
	client.URL = "http://127.0.0.1:1/must-not-be-requested"
	snapshot := client.ReadOnlySnapshot()
	provider := snapshot["route"]
	provider.Env[0] = "changed"
	model := provider.Models["m"]
	model.Modalities.Input[0] = "changed"
	model.Limit.Context = 1
	provider.Models["m"] = model
	delete(snapshot, "route")
	again := client.ReadOnlySnapshot()
	if again["route"].Env[0] != "KEY" || again["route"].Models["m"].Limit.Context != 128000 || again["route"].Models["m"].Modalities.Input[0] != "text" {
		t.Fatal("snapshot exposed mutable catalog state")
	}
	if (*Client)(nil).ReadOnlySnapshot() != nil {
		t.Fatal("nil client should have no cached catalog")
	}
}
