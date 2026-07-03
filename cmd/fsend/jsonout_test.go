package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
)

// A failure before any transfer path is entered (flag-parse error) has no
// role; the done event must omit the field rather than fabricate "receiver".
func TestJSONDoneFromErr_RoleOmittedWhenUnknown(t *testing.T) {
	b, err := json.Marshal(jsonDoneFromErr(fserrors.ErrUsage, 24, ""))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"role"`) {
		t.Errorf("undetermined role must be omitted, got %s", b)
	}
	if !strings.Contains(string(b), `"error":"E024"`) {
		t.Errorf("catalog code missing: %s", b)
	}

	b, err = json.Marshal(jsonDoneFromErr(fserrors.ErrCodeNotFound, 2, "receiver"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"role":"receiver"`) {
		t.Errorf("determined role must be present, got %s", b)
	}
}
