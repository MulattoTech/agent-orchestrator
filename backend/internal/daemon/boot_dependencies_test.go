package daemon

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
)

// These tests never enter the daemon lifecycle. Every supplied callback is an
// inert counter; deleting any required dependency must fail before callbacks.
func TestBootDependenciesRejectMissingFactoryBeforeEffects(t *testing.T) {
	dependencyType := reflect.TypeOf(bootDependencies{})
	for missing := 0; missing < dependencyType.NumField(); missing++ {
		name := dependencyType.Field(missing).Name
		t.Run(name, func(t *testing.T) {
			calls := 0
			value := reflect.New(dependencyType).Elem()
			for i := 0; i < dependencyType.NumField(); i++ {
				if i == missing {
					continue
				}
				field := value.Field(i)
				functionType := field.Type()
				if functionType.Kind() != reflect.Func {
					t.Fatalf("new dependency %s needs an explicit completeness rule", dependencyType.Field(i).Name)
				}
				field.Set(reflect.MakeFunc(functionType, func([]reflect.Value) []reflect.Value {
					calls++
					outputs := make([]reflect.Value, functionType.NumOut())
					for j := range outputs {
						outputs[j] = reflect.Zero(functionType.Out(j))
					}
					return outputs
				}))
			}
			dependencies := value.Interface().(bootDependencies)
			if err := runWithDependencies("offline-missing-dependency", dependencies); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("missing %s error=%v", name, err)
			}
			if calls != 0 {
				t.Fatalf("missing %s invoked %d effect callbacks", name, calls)
			}
		})
	}
}

func TestProductionBootFactoryDeclaresEveryDependency(t *testing.T) {
	// Building this set creates function values only, not their services.
	if err := productionBootDependencies().validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMobileBootEffectsRequireEveryCollaborator(t *testing.T) {
	effectType := reflect.TypeOf(mobileBootEffects{})
	for missing := 0; missing < effectType.NumField(); missing++ {
		name := effectType.Field(missing).Name
		t.Run(name, func(t *testing.T) {
			calls := 0
			value := reflect.New(effectType).Elem()
			for i := 0; i < effectType.NumField(); i++ {
				if i == missing {
					continue
				}
				field := value.Field(i)
				functionType := field.Type()
				field.Set(reflect.MakeFunc(functionType, func([]reflect.Value) []reflect.Value {
					calls++
					outputs := make([]reflect.Value, functionType.NumOut())
					for j := range outputs {
						outputs[j] = reflect.Zero(functionType.Out(j))
					}
					return outputs
				}))
			}
			if err := value.Interface().(mobileBootEffects).validate(); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("missing %s error=%v", name, err)
			}
			if calls != 0 {
				t.Fatalf("validation invoked %d effects", calls)
			}
		})
	}
}

func TestBootValueMissingRejectsTypedNil(t *testing.T) {
	var pointer *int
	var function func()
	if !bootValueMissing(pointer) || !bootValueMissing(function) || !bootValueMissing(nil) {
		t.Fatal("typed nil accepted")
	}
	if bootValueMissing(1) || bootValueMissing(func() {}) {
		t.Fatal("present value rejected")
	}
}
func TestBootDependenciesRejectNonpositiveAccountLimitsIntervalBeforeLifecycle(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		t.Run(interval.String(), func(t *testing.T) {
			dependencies := productionBootDependencies()
			dependencies.LoadConfig = func() (config.Config, error) { return config.Config{}, nil }
			dependencies.AccountLimitsInterval = func() time.Duration { return interval }
			dependencies.Observe = func(bootEvent) { t.Fatal("invalid interval entered the daemon lifecycle") }
			err := runWithDependencies("invalid-offline-interval", dependencies)
			if err == nil || !strings.Contains(err.Error(), "interval must be positive") {
				t.Fatalf("invalid interval error=%v", err)
			}
		})
	}
	if got := productionBootDependencies().AccountLimitsInterval(); got != time.Minute {
		t.Fatalf("production polling interval changed: %s", got)
	}
}
