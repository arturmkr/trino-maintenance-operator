// Package argocd tests the ArgoCD custom health check for TrinoMaintenance with
// the same Lua interpreter ArgoCD embeds (gopher-lua).
package argocd

import (
	"os"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// run evaluates health.lua against obj and returns (status, message).
func run(t *testing.T, obj map[string]any) (string, string) {
	t.Helper()
	script, err := os.ReadFile("health.lua")
	if err != nil {
		t.Fatal(err)
	}
	L := lua.NewState()
	defer L.Close()
	L.SetGlobal("obj", toLua(L, obj))
	if err := L.DoString(string(script)); err != nil {
		t.Fatalf("lua error: %v", err)
	}
	hs, ok := L.Get(-1).(*lua.LTable)
	if !ok {
		t.Fatalf("script did not return a table, got %T", L.Get(-1))
	}
	return hs.RawGetString("status").String(), hs.RawGetString("message").String()
}

func toLua(L *lua.LState, v any) lua.LValue {
	switch x := v.(type) {
	case nil:
		return lua.LNil
	case string:
		return lua.LString(x)
	case bool:
		return lua.LBool(x)
	case int:
		return lua.LNumber(x)
	case float64:
		return lua.LNumber(x)
	case map[string]any:
		tbl := L.NewTable()
		for k, val := range x {
			tbl.RawSetString(k, toLua(L, val))
		}
		return tbl
	case []any:
		tbl := L.NewTable()
		for _, val := range x {
			tbl.Append(toLua(L, val))
		}
		return tbl
	default:
		panic("unsupported type")
	}
}

func maintenance(phase, message string) map[string]any {
	obj := map[string]any{
		"apiVersion": "trino.example.io/v1alpha1",
		"kind":       "TrinoMaintenance",
		"metadata":   map[string]any{"name": "general-1-drain", "namespace": "trino"},
		"spec":       map[string]any{"clusterName": "general-1", "action": "Drain"},
	}
	if phase != "" {
		status := map[string]any{"phase": phase}
		if message != "" {
			status["message"] = message
		}
		obj["status"] = status
	}
	return obj
}

func TestHealth(t *testing.T) {
	tests := []struct {
		phase, message string
		wantStatus     string
		wantMessage    string
	}{
		{"", "", "Progressing", "Waiting for trino-maintenance-operator"},
		{"Waiting", "routing group busy", "Progressing", "Waiting: routing group busy"},
		{"Draining", "5 running", "Progressing", "Draining: 5 running"},
		{"Drained", "no queries", "Healthy", "Drained: no queries"},
		{"Resuming", "", "Progressing", "Resuming"},
		{"Completed", "backend active", "Healthy", "Completed: backend active"},
		{"Failed", "gateway 503", "Degraded", "Failed: gateway 503"},
		{"SomethingNew", "", "Progressing", "SomethingNew"},
	}
	for _, tt := range tests {
		name := tt.phase
		if name == "" {
			name = "no-status"
		}
		t.Run(name, func(t *testing.T) {
			obj := maintenance(tt.phase, tt.message)
			if tt.phase == "Resuming" || tt.phase == "Completed" {
				obj["spec"] = map[string]any{"action": "Resume"}
			}
			status, message := run(t, obj)
			if status != tt.wantStatus {
				t.Fatalf("status = %q, want %q", status, tt.wantStatus)
			}
			if message != tt.wantMessage {
				t.Fatalf("message = %q, want %q", message, tt.wantMessage)
			}
		})
	}
}

func TestHealthWithoutStatusField(t *testing.T) {
	obj := map[string]any{"kind": "TrinoMaintenance", "status": map[string]any{}}
	if status, _ := run(t, obj); status != "Progressing" {
		t.Fatalf("status = %q, want Progressing", status)
	}
}

func TestWrongTerminalPhaseCannotUnblockHook(t *testing.T) {
	if status, _ := run(t, maintenance("Completed", "wrong action")); status == "Healthy" {
		t.Fatal("Resume success must not complete PreSync")
	}
}
