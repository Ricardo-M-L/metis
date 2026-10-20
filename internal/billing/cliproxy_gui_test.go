package billing

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

func TestCLIProxyGUIReadOnlyExactRequestTokenRecovery(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 unavailable")
	}
	db := filepath.Join(t.TempDir(), "usage.db")
	schema := `CREATE TABLE usage_events(event_key TEXT,request_id TEXT,provider TEXT,model TEXT,alias TEXT,user_agent TEXT,failed INTEGER,canceled INTEGER,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_creation_tokens INTEGER); INSERT INTO usage_events VALUES('event1','gateway-id','codex','gpt-6-sol','gpt-6-sol','existing-client/1 metis-usage/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',0,0,100,20,30,0);`
	if out, err := exec.Command(sqlite, db, schema).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	endpoint := "http://127.0.0.1:8317/v1/responses"
	source, err := NewCLIProxyGUI(CLIProxyGUIConfig{Database: db, SQLiteExecutable: sqlite, InferenceEndpoint: endpoint, LedgerProvider: "easycliproxyapi"})
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := EndpointSHA256(endpoint)
	request := Request{Provider: "easycliproxyapi", EndpointSHA256: hash, Model: "gpt-6-sol", ClientRequestID: strings.Repeat("a", 32), ProviderRequestID: "gateway-id", ResponseID: "resp-1"}
	observation, err := source.Lookup(context.Background(), request)
	if err != nil || observation.Receipt.ActualUSD != nil || observation.Receipt.Usage != (projectcoord.TokenUsage{Input: 70, Output: 20, CacheRead: 30}) {
		t.Fatalf("bad token-only recovery %+v %v", observation, err)
	}
	if strings.Contains(string(observation.Evidence), "api_key") {
		t.Fatal("copied private API key metadata")
	}
	request.ProviderRequestID = "missing"
	if _, err := source.Lookup(context.Background(), request); err == nil || errors.Is(err, ErrPending) {
		t.Fatal("accepted a contradictory gateway header ID")
	}
	request.ProviderRequestID = ""
	if recovered, err := source.Lookup(context.Background(), request); err != nil || recovered.Receipt.ProviderRequestID != "gateway-id" {
		t.Fatal("could not recover exact client correlation before response headers")
	}
	request.ProviderRequestID = "gateway-id' OR 1=1 --"
	if _, err := source.Lookup(context.Background(), request); err == nil {
		t.Fatal("accepted a contradictory gateway identity")
	}
	request.ProviderRequestID = ""
	request.ClientRequestID = strings.Repeat("b", 32)
	if _, err := source.Lookup(context.Background(), request); !errors.Is(err, ErrPending) {
		t.Fatal("matched another client's user agent marker")
	}
	request.ClientRequestID = strings.Repeat("a", 32)
	request.ProviderRequestID = "gateway-id"
	request.Model = "other-model"
	if _, err := source.Lookup(context.Background(), request); err == nil {
		t.Fatal("matched wrong model")
	}
	request.Model = "gpt-6-sol"
	if out, err := exec.Command(sqlite, db, `INSERT INTO usage_events SELECT * FROM usage_events;`).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := source.Lookup(context.Background(), request); err == nil || errors.Is(err, ErrPending) {
		t.Fatal("accepted ambiguous duplicate gateway request")
	}
}

func TestCLIProxyGUIRejectsMissingPartialOrRepeatedClientMarkers(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 unavailable")
	}
	marker := "metis-usage/" + strings.Repeat("a", 32)
	for _, agent := range []string{"Go-http-client/1.1", "prefix" + marker, marker + "suffix", marker + " " + marker} {
		t.Run(agent, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "usage.db")
			query := `CREATE TABLE usage_events(event_key TEXT,request_id TEXT,provider TEXT,model TEXT,alias TEXT,user_agent TEXT,failed INTEGER,canceled INTEGER,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_creation_tokens INTEGER); INSERT INTO usage_events VALUES('event','gateway-id','codex','model','model','` + agent + `',0,0,10,2,0,0);`
			if out, err := exec.Command(sqlite, db, query).CombinedOutput(); err != nil {
				t.Fatalf("%v %s", err, out)
			}
			endpoint := "http://127.0.0.1:8317/v1/responses"
			hash, _ := EndpointSHA256(endpoint)
			source, _ := NewCLIProxyGUI(CLIProxyGUIConfig{Database: db, SQLiteExecutable: sqlite, InferenceEndpoint: endpoint, LedgerProvider: "local"})
			if _, err := source.Lookup(context.Background(), Request{Provider: "local", EndpointSHA256: hash, Model: "model", ClientRequestID: strings.Repeat("a", 32), ProviderRequestID: "gateway-id"}); err == nil {
				t.Fatal("accepted unrelated/ambiguous client marker")
			}
		})
	}
}

func TestCLIProxyGUIIncompleteFailedOrInvalidTokenEvidenceStaysUnknown(t *testing.T) {
	sqlite, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Skip("sqlite3 unavailable")
	}
	for _, value := range []string{"1,0,100,20,30,0", "0,1,100,20,30,0", "0,0,NULL,20,30,0", "0,0,10,20,30,0"} {
		t.Run(value, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "usage.db")
			query := `CREATE TABLE usage_events(event_key TEXT,request_id TEXT,provider TEXT,model TEXT,alias TEXT,user_agent TEXT,failed INTEGER,canceled INTEGER,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_creation_tokens INTEGER); INSERT INTO usage_events VALUES('event1','gateway-id','codex','model','model','metis-usage/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',` + value + `);`
			if out, err := exec.Command(sqlite, db, query).CombinedOutput(); err != nil {
				t.Fatalf("%v %s", err, out)
			}
			endpoint := "http://127.0.0.1:8317/v1/responses"
			hash, _ := EndpointSHA256(endpoint)
			source, _ := NewCLIProxyGUI(CLIProxyGUIConfig{Database: db, SQLiteExecutable: sqlite, InferenceEndpoint: endpoint, LedgerProvider: "local"})
			if _, err := source.Lookup(context.Background(), Request{Provider: "local", EndpointSHA256: hash, Model: "model", ClientRequestID: strings.Repeat("a", 32), ProviderRequestID: "gateway-id"}); err == nil {
				t.Fatal("settled incomplete/invalid source")
			}
		})
	}
}
