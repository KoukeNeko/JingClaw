package storage_test

import (
	"reflect"
	"testing"

	"github.com/KoukeNeko/JingClaw/core/internal/domain"
	"github.com/KoukeNeko/JingClaw/core/internal/domain/domaintest"
	"github.com/KoukeNeko/JingClaw/core/internal/storage"
)

// A payload the codec does not know is dropped on a real database while every
// in-memory test keeps passing. That has happened once; this is what stops it
// happening again.
func TestEveryEventKindRoundTrips(t *testing.T) {
	samples := domaintest.Payloads()

	for _, kind := range domain.AllEventKinds() {
		payload, ok := samples[kind]
		if !ok {
			t.Errorf("no sample for %s; add one to domaintest.Payloads", kind)
			continue
		}

		t.Run(string(kind), func(t *testing.T) {
			raw, err := storage.EncodePayload(payload)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}

			decoded, err := storage.DecodePayload(kind, raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}

			// Field by field, not just "it came back": a codec that silently
			// drops a field it does not know about is the same defect in a
			// quieter form.
			if !reflect.DeepEqual(decoded, payload) {
				t.Errorf("round trip changed the payload:\n got %+v\nwant %+v", decoded, payload)
			}
		})
	}
}

// Sending an event the wire format cannot express would corrupt every client's
// view, so an unknown kind has to fail rather than pass through empty.
func TestAnUnknownKindIsRefused(t *testing.T) {
	if _, err := storage.DecodePayload("something.invented", []byte("{}")); err == nil {
		t.Error("the codec accepted a kind it does not know")
	}
}
