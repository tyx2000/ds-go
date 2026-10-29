package context

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestOfficialInstructionRendererGolden(t *testing.T) {
	data, err := os.ReadFile("testdata/instruction-render-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Cases []struct {
			Name, Kind string
			Files      []InstructionFile
			Items      []InstructionChangeItem
			MaxBytes   int
			Replace    bool
			Expected   json.RawMessage
		}
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, test := range corpus.Cases {
		t.Run(test.Name, func(t *testing.T) {
			var got any
			if test.Kind == "baseline" {
				rendered, included := RenderInstructionBaseline(test.Files, test.MaxBytes, test.Replace)
				paths := []string{}
				for _, file := range included {
					paths = append(paths, file.AbsolutePath)
				}
				got = struct {
					RenderedInstructions
					Included []string `json:"included"`
				}{rendered, paths}
			} else {
				text, changes := RenderInstructionChanges(test.Items, test.MaxBytes)
				got = struct {
					Text    string              `json:"text"`
					Changes []InstructionChange `json:"changes"`
				}{text, changes}
			}
			var actual, expected any
			encoded, _ := json.Marshal(got)
			_ = json.Unmarshal(encoded, &actual)
			_ = json.Unmarshal(test.Expected, &expected)
			if !reflect.DeepEqual(actual, expected) {
				t.Fatalf("renderer differs\ngot %s\nwant %s", encoded, test.Expected)
			}
		})
	}
}
