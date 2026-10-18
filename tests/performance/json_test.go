package performance

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestEvaluateFioUnicodeExtensionKeys(t *testing.T) {
	for _, pair := range [][2]string{
		{"k", "\u212a"}, {"sigma-\u03c3", "sigma-\u03c2"}, {"s", "\u017f"},
		{"i", "\u0130"}, {"a", "\u03b1"}, {"e", "\u00e9"},
	} {
		t.Run(pair[0]+"/"+pair[1], func(t *testing.T) {
			raw := editOutput(t, fixture(t, "fio-success.json"), func(document map[string]any) {
				document["extension"] = map[string]int{pair[0]: 1, pair[1]: 2}
			})
			result := EvaluateFio(testManifest(t), successfulProcess(), raw)
			if strings.EqualFold(pair[0], pair[1]) {
				if result.Status != Fail || !hasCode(result, "OUTPUT_INVALID") {
					t.Fatal("Unicode case collision accepted in extension object")
				}
			} else if result.Status != Pass {
				t.Fatal("distinct Unicode extension keys rejected")
			}
		})
	}
}

func FuzzJSONKeyCollisions(f *testing.F) {
	for _, pair := range [][2]string{{"short_ios", "\u017fhort_ios"}, {"k", "\u212a"}, {"i", "\u0130"}, {"a", "A"}, {"a", "b"}} {
		f.Add(pair[0], pair[1])
	}
	f.Fuzz(func(t *testing.T, left, right string) {
		if len(left)+len(right) > 4096 {
			return
		}
		encoded, err := json.Marshal([]string{left, right})
		if err != nil {
			t.Fatal(err)
		}
		var decoded [2]string
		if decodeErr := json.Unmarshal(encoded, &decoded); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		var document bytes.Buffer
		document.WriteByte('{')
		for index, key := range decoded {
			if index != 0 {
				document.WriteByte(',')
			}
			rawKey, marshalErr := json.Marshal(key)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			document.Write(rawKey)
			document.WriteString(":0")
		}
		document.WriteByte('}')
		rejected := checkJSON(document.Bytes(), maxManifestBytes) != nil
		if rejected != strings.EqualFold(decoded[0], decoded[1]) {
			t.Fatal("ambiguous-key guard disagrees with Go JSON field folding")
		}
	})
}
