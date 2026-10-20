package learning

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLearningSchemaWritesV2AndReadsV1WithoutMutation(t *testing.T) {
	s, _ := stageTest(t)
	st, err := s.Read("test-skill")
	if err != nil {
		t.Fatal(err)
	}
	if st.Schema != 2 {
		t.Fatalf("new writer did not fence old schema-1 CLI: schema=%d", st.Schema)
	}
	st.Schema = 1
	legacy, _ := json.MarshalIndent(st, "", "  ")
	path := s.path("test-skill")
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	read, err := s.Read("test-skill")
	if err != nil || read.Schema != 1 {
		t.Fatalf("cannot read legacy schema: %+v %v", read, err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(legacy, after) {
		t.Fatal("read-only inspection migrated legacy state")
	}
	if _, err := s.Stage(context.Background(), "test-skill", nil, []byte("new candidate after migration"), "host"); err != nil {
		t.Fatal(err)
	}
	read, err = s.Read("test-skill")
	if err != nil || read.Schema != 2 {
		t.Fatalf("mutation failed to fence old writer: %d %v", read.Schema, err)
	}
	read.Schema = 3
	future, _ := json.Marshal(read)
	if err := os.WriteFile(path, future, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read("test-skill"); err == nil {
		t.Fatal("future learning schema silently accepted")
	}
}
func TestControllerSchemaWritesV2AndReadsV1WithoutMutation(t *testing.T) {
	c, _, _, p := controllerFixture(t, false)
	st, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Schema != 2 {
		t.Fatalf("controller writer did not fence old schema-1 CLI: schema=%d", st.Schema)
	}
	st.Schema = 1
	legacy, _ := json.MarshalIndent(st, "", "  ")
	path := filepath.Join(c.Dir(), "state.json")
	if err := os.WriteFile(path, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	read, err := c.Read()
	if err != nil || read.Schema != 1 {
		t.Fatalf("cannot inspect legacy controller: %d %v", read.Schema, err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(legacy, after) {
		t.Fatal("controller read migrated durable state")
	}
	p.MaxEvaluations++
	if _, err := c.Configure(context.Background(), p.ControllerPolicySpec); err != nil {
		t.Fatal(err)
	}
	read, err = c.Read()
	if err != nil || read.Schema != 2 {
		t.Fatalf("controller mutation did not write schema 2: %d %v", read.Schema, err)
	}
	read.Schema = 3
	future, _ := json.Marshal(read)
	if err := os.WriteFile(path, future, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(); err == nil {
		t.Fatal("future controller schema silently accepted")
	}
}
