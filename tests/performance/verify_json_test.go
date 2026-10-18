package performance

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestDecodeEvidenceRecordRejectsAmbiguousJSON(t *testing.T) {
	valid, err := json.Marshal(successfulProcess())
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"null":             []byte("null"),
		"array":            []byte("[]"),
		"second document":  append(bytes.Clone(valid), valid...),
		"duplicate":        bytes.Replace(valid, []byte(`"canceled":false`), []byte(`"canceled":true,"canceled":false`), 1),
		"case collision":   bytes.Replace(valid, []byte(`"canceled":false`), []byte(`"Canceled":true,"canceled":false`), 1),
		"wrong case":       bytes.Replace(valid, []byte(`"canceled"`), []byte(`"Canceled"`), 1),
		"unknown field":    bytes.Replace(valid, []byte(`"canceled":false`), []byte(`"unreviewed":false`), 1),
		"integer overflow": bytes.Replace(valid, []byte(`"exit_code":0`), []byte(`"exit_code":18446744073709551616`), 1),
		"oversized":        bytes.Repeat([]byte(" "), maxProcessBytes+1),
		"depth":            []byte(`{"nested":` + string(bytes.Repeat([]byte("["), 66)) + "0" + string(bytes.Repeat([]byte("]"), 66)) + "}"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, decodeErr := decodeRecord[ProcessReceipt](raw, maxProcessBytes); !errors.Is(decodeErr, ErrEvidenceCorrupt) {
				t.Fatal("ambiguous record accepted")
			}
		})
	}
}

func FuzzDecodeEvidenceRecord(f *testing.F) {
	seed, err := json.Marshal(CaptureResult{SchemaVersion: EvidenceVersion, Evaluation: Evaluation{Status: Pass}})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte(`{"schema_version":1,"index":null,"artifacts":[],"evaluation":{"verified_bytes":null,"status":"FAIL","findings":[{"code":"OUTPUT_INVALID"}],"jobs":null}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		value, decodeErr := decodeRecord[CaptureResult](raw, maxIndexBytes)
		if decodeErr != nil {
			return
		}
		encoded, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			t.Fatal("decoded record cannot be serialized")
		}
		roundTrip, roundTripErr := decodeRecord[CaptureResult](encoded, maxIndexBytes)
		if roundTripErr != nil || !reflect.DeepEqual(value, roundTrip) {
			t.Fatal("record round trip changed typed observations")
		}
	})
}
