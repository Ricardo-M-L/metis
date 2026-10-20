// Package billing reads authoritative request records. Catalogue prices are
// deliberately absent: token evidence and monetary charges are different data.
package billing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"

	"github.com/Ricardo-M-L/metis/internal/projectcoord"
)

var ErrPending = errors.New("billing: authoritative request record is not available yet")

const EvidenceLimit = 1 << 20

type Request struct {
	Provider, EndpointSHA256, Model, ClientRequestID, ProviderRequestID, ResponseID string
}

type Observation struct {
	Receipt  projectcoord.UsageReceipt
	Evidence []byte
}

type Source interface {
	Lookup(context.Context, Request) (Observation, error)
}

func failure(code string) error { return errors.New("billing: " + code) }

// EndpointSHA256 matches the transport ledger's URL canonicalization. Query
// parameters and credentials are never part of the stored endpoint identity.
func EndpointSHA256(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", failure("invalid_inference_endpoint")
	}
	sum := sha256.Sum256([]byte(strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + u.EscapedPath()))
	return hex.EncodeToString(sum[:]), nil
}

func bind(request Request, provider, endpoint string) error {
	if provider == "" || request.Provider != provider || request.EndpointSHA256 != endpoint || request.ClientRequestID == "" || request.Model == "" {
		return failure("request_scope_mismatch")
	}
	return nil
}

func observation(request Request, authority string, usage projectcoord.TokenUsage, actual *float64, evidence []byte) (Observation, error) {
	if usage.Input < 0 || usage.Output < 0 || usage.CacheRead < 0 || usage.CacheWrite < 0 {
		return Observation{}, failure("invalid_token_counts")
	}
	receipt := projectcoord.UsageReceipt{Authority: authority, Provider: request.Provider, EndpointSHA256: request.EndpointSHA256, Model: request.Model, ClientRequestID: request.ClientRequestID, ProviderRequestID: request.ProviderRequestID, ResponseID: request.ResponseID, Usage: usage, ActualUSD: actual}
	// The ID describes the accounting revision, rather than incidental HTTP
	// metadata. Polling a record whose latency or timestamp changes is a no-op.
	semantic, err := json.Marshal(receipt)
	if err != nil {
		return Observation{}, failure("invalid_charge")
	}
	id, raw := sha256.Sum256(semantic), sha256.Sum256(evidence)
	receipt.ReceiptID = authority + ":" + hex.EncodeToString(id[:])
	receipt.EvidenceSHA256 = hex.EncodeToString(raw[:])
	return Observation{Receipt: receipt, Evidence: evidence}, nil
}

// uniqueJSON rejects duplicate keys, including case variants. Unknown provider
// fields are allowed, but cannot shadow a charge or identity field.
func uniqueJSON(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	var value func() error
	value = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		if delimiter, ok := t.(json.Delim); ok {
			switch delimiter {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, err := d.Token()
					if err != nil {
						return err
					}
					key, ok := k.(string)
					if !ok || seen[strings.ToLower(key)] {
						return failure("duplicate_json_field")
					}
					seen[strings.ToLower(key)] = true
					if err := value(); err != nil {
						return err
					}
				}
				_, err = d.Token()
				return err
			case '[':
				for d.More() {
					if err := value(); err != nil {
						return err
					}
				}
				_, err = d.Token()
				return err
			default:
				return failure("invalid_json")
			}
		}
		return nil
	}
	if err := value(); err != nil {
		return failure("invalid_json")
	}
	if _, err := d.Token(); err != io.EOF {
		return failure("invalid_json")
	}
	return nil
}
