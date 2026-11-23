package acp

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCatchAllCommonFieldsHaveOneOwner(t *testing.T) {
	var request CreateElicitationRequest
	if err := json.Unmarshal([]byte(`{"message":"before","_meta":{"revision":1},"mode":"_custom","sessionId":"s","payload":{"future":true}}`), &request); err != nil {
		t.Fatal(err)
	}
	other := request.Value.(*CreateElicitationRequestOther)
	for _, name := range []string{"message", "_meta", "mode"} {
		if _, duplicate := other.Extra[name]; duplicate {
			t.Fatalf("declared field %q was retained as extension data", name)
		}
	}
	request.Message = "after"
	meta, err := NewMeta(map[string]any{"revision": 2})
	if err != nil {
		t.Fatal(err)
	}
	request.Meta = OptValue(meta)
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"message":`, `"_meta":`, `"mode":`} {
		if bytes.Count(encoded, []byte(key)) != 1 {
			t.Errorf("%s has multiple owners in %s", key, encoded)
		}
	}
	var received struct {
		Message string `json:"message"`
		Meta    struct {
			Revision int `json:"revision"`
		} `json:"_meta"`
		Payload map[string]bool `json:"payload"`
	}
	if err := json.Unmarshal(encoded, &received); err != nil {
		t.Fatal(err)
	}
	if received.Message != "after" || received.Meta.Revision != 2 || !received.Payload["future"] {
		t.Fatalf("mutated common fields or unknown payload did not survive: %s", encoded)
	}

	var response CreateElicitationResponse
	if err := json.Unmarshal([]byte(`{"action":"_custom","_meta":{"revision":1},"future":true}`), &response); err != nil {
		t.Fatal(err)
	}
	response.Meta = OptValue(meta)
	encoded, err = json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(encoded, []byte(`"_meta":`)) != 1 || !bytes.Contains(encoded, []byte(`"revision":2`)) {
		t.Fatalf("response metadata has competing owners: %s", encoded)
	}
}

func TestCatchAllCannotOverrideDeclaredProperties(t *testing.T) {
	for _, test := range []struct {
		name string
		keys []string
		new  func(map[string]json.RawMessage) any
	}{
		{"request", []string{"mode", "message", "_meta"}, func(extra map[string]json.RawMessage) any {
			extra["sessionId"] = json.RawMessage(`"s"`)
			return &CreateElicitationRequestOther{Mode: "_custom", Extra: extra}
		}},
		{"response", []string{"action", "_meta"}, func(extra map[string]json.RawMessage) any {
			return &CreateElicitationResponseOther{Action: "_custom", Extra: extra}
		}},
		{"property", []string{"type"}, func(extra map[string]json.RawMessage) any {
			return &ElicitationPropertySchemaOther{Type: "_custom", Extra: extra}
		}},
		{"items", []string{"type"}, func(extra map[string]json.RawMessage) any {
			return &MultiSelectItemsOther{Type: "_custom", Extra: extra}
		}},
	} {
		for _, key := range test.keys {
			t.Run(test.name+"/"+key, func(t *testing.T) {
				value := test.new(map[string]json.RawMessage{key: json.RawMessage(`"accept"`)})
				if encoded, err := json.Marshal(value); err == nil {
					t.Fatalf("reserved key %q encoded as extension data: %s", key, encoded)
				}
			})
		}
	}
}

func TestIntegerFieldsAcceptIntegralRepresentations(t *testing.T) {
	for _, literal := range []string{"1.0", "1e0"} {
		t.Run(literal, func(t *testing.T) {
			var initialize InitializeRequest
			if err := json.Unmarshal([]byte(`{"protocolVersion":`+literal+`}`), &initialize); err != nil {
				t.Fatal(err)
			}
			if initialize.ProtocolVersion != 1 {
				t.Fatalf("protocolVersion = %d", initialize.ProtocolVersion)
			}
			var file ReadTextFileRequest
			if err := json.Unmarshal([]byte(`{"sessionId":"s","path":"/file","line":`+literal+`}`), &file); err != nil {
				t.Fatal(err)
			}
			if line, present := file.Line.Get(); !present || line != 1 {
				t.Fatalf("line = %d, present = %v", line, present)
			}
			var cancellation cancelRequestNotification
			if err := json.Unmarshal([]byte(`{"requestId":`+literal+`}`), &cancellation); err != nil {
				t.Fatal(err)
			}
			id, ok := cancellation.RequestID.(*requestIDNumber)
			if !ok || *id != 1 {
				t.Fatalf("requestId = %#v", cancellation.RequestID)
			}
		})
	}
}

func TestNamedScalarsRejectNull(t *testing.T) {
	for _, target := range []any{
		new(SessionID), new(ProtocolVersion), new(ErrorCode), new(SessionConfigOptionCategory), new(CompactionID),
	} {
		if err := json.Unmarshal([]byte("null"), target); err == nil {
			t.Errorf("%T accepted null", target)
		}
	}
}

func TestOperationalFieldsRejectMalformedValues(t *testing.T) {
	for _, test := range []struct {
		name, raw string
		target    any
	}{
		{"terminal args", `{"sessionId":"s","command":"tool","args":["--safe",17,"file"]}`, &CreateTerminalRequest{}},
		{"terminal cwd", `{"sessionId":"s","command":"tool","cwd":17}`, &CreateTerminalRequest{}},
		{"terminal output limit", `{"sessionId":"s","command":"tool","outputByteLimit":"broken"}`, &CreateTerminalRequest{}},
		{"file limit", `{"sessionId":"s","path":"/file","limit":"broken"}`, &ReadTextFileRequest{}},
		{"session MCP", `{"cwd":"/workspace","mcpServers":[{"command":17}]}`, &NewSessionRequest{}},
		{"auth environment", `{"id":"auth","name":"Auth","env":{"KEY":null}}`, &AuthMethodTerminal{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(test.raw), test.target); err == nil {
				t.Fatalf("malformed operational input accepted: %s", test.raw)
			}
		})
	}
}

func TestOperationalNullDefaultsPreservePresence(t *testing.T) {
	var server McpServerStdio
	if err := json.Unmarshal([]byte(`{"name":"m","command":"/tool","args":null,"env":null}`), &server); err != nil {
		t.Fatal(err)
	}
	if server.Args == nil || server.Env == nil || len(server.Args) != 0 || len(server.Env) != 0 {
		t.Fatalf("null required lists did not normalize to empty: %#v", server)
	}
	if err := json.Unmarshal([]byte(`{"name":"m","command":"/tool","env":[]}`), &server); err == nil {
		t.Fatal("a null default made a required property optional")
	}

	var authentication AuthMethodTerminal
	if err := json.Unmarshal([]byte(`{"id":"auth","name":"Auth","args":null,"env":null}`), &authentication); err != nil {
		t.Fatal(err)
	}
	environment, present := authentication.Env.Get()
	if authentication.Args == nil || !present || environment == nil || len(environment) != 0 {
		t.Fatalf("null authentication defaults lost presence: %#v", authentication)
	}
	encoded, err := json.Marshal(authentication)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"args":[]`)) || !bytes.Contains(encoded, []byte(`"env":{}`)) {
		t.Fatalf("null defaults encoded incorrectly: %s", encoded)
	}

	if err := json.Unmarshal([]byte(`{"id":"auth","name":"Auth"}`), &authentication); err != nil {
		t.Fatal(err)
	}
	if authentication.Args != nil || !authentication.Env.IsZero() {
		t.Fatalf("omitted options acquired present defaults: %#v", authentication)
	}
}

func TestNoticeTitleRequiresContent(t *testing.T) {
	for _, title := range []string{"", "提醒"} {
		notice := Notice{Severity: NoticeSeverityInfo, Title: title}
		encoded, err := json.Marshal(notice)
		if title == "" {
			if err == nil {
				t.Fatal("encoded an empty notice title")
			}
			if err := json.Unmarshal([]byte(`{"severity":"info","title":""}`), &notice); err == nil {
				t.Fatal("decoded an empty notice title")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &notice); err != nil || notice.Title != title {
			t.Fatalf("notice title = %q, %v", notice.Title, err)
		}
	}
}

func TestMixedUnionsTryEverySchemaAlternative(t *testing.T) {
	t.Run("authentication", func(t *testing.T) {
		value, err := unmarshalAuthMethod([]byte(`{"type":"terminal","id":"cli","name":"CLI","env":["invalid"]}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := value.(*AuthMethodAgent); !ok {
			t.Fatalf("decoded %T, want the matching untagged agent arm", value)
		}
	})
	t.Run("MCP server", func(t *testing.T) {
		value, err := unmarshalMcpServer([]byte(`{"type":"http","name":"mcp","command":"/tool","args":[],"env":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := value.(*McpServerStdio); !ok {
			t.Fatalf("decoded %T, want the matching untagged stdio arm", value)
		}
	})
	t.Run("configuration", func(t *testing.T) {
		var value SetSessionConfigOptionRequest
		if err := json.Unmarshal([]byte(`{"sessionId":"s","configId":"c","type":"boolean","value":"enabled"}`), &value); err != nil {
			t.Fatal(err)
		}
		if _, ok := value.Value.(*SetSessionConfigOptionRequestValueID); !ok {
			t.Fatalf("decoded %T, want the matching untagged value ID arm", value.Value)
		}
	})
	t.Run("multi-select items", func(t *testing.T) {
		value, err := unmarshalMultiSelectItems([]byte(`{"type":"string","anyOf":[{"const":"x","title":"X"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := value.(*TitledMultiSelectItems); !ok {
			t.Fatalf("decoded %T, want the matching untagged titled arm", value)
		}
	})
}
