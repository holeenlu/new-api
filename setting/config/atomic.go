package config

import (
	"fmt"
	"reflect"
	"sync/atomic"
)

type managedConfig interface {
	snapshotForConfig() (interface{}, error)
	validateConfigMap(configMap map[string]string) error
	updateFromConfigMap(configMap map[string]string) error
}

// AtomicConfig stores an immutable configuration snapshot. It is intended for
// top-level struct values whose fields are exported and read from request or
// background goroutines while the configuration manager applies live database
// updates. Top-level fields may be scalars or pure-value structs; nested structs
// may also contain arrays. Pointer, map, slice, interface, function, and channel
// fields are rejected by NewAtomicConfig because a shallow snapshot could
// otherwise alias mutable storage and reintroduce a data race.
type AtomicConfig[T any] struct {
	value    atomic.Pointer[T]
	validate func(T) error
}

// NewAtomicConfig creates a managed configuration initialized with a complete
// immutable snapshot.
func NewAtomicConfig[T any](initial T) *AtomicConfig[T] {
	return newAtomicConfig(initial, nil)
}

// NewValidatedAtomicConfig creates an immutable configuration whose complete
// snapshot must also satisfy domain validation before it can be published.
func NewValidatedAtomicConfig[T any](initial T, validate func(T) error) *AtomicConfig[T] {
	if validate == nil {
		panic("config.NewValidatedAtomicConfig requires a validator")
	}
	return newAtomicConfig(initial, validate)
}

func newAtomicConfig[T any](initial T, validate func(T) error) *AtomicConfig[T] {
	typeOfT := reflect.TypeOf((*T)(nil)).Elem()
	if err := validateAtomicConfigType(typeOfT); err != nil {
		panic(err)
	}
	if validate != nil {
		if err := validate(initial); err != nil {
			panic(fmt.Errorf("invalid initial atomic configuration: %w", err))
		}
	}
	config := &AtomicConfig[T]{validate: validate}
	config.store(initial)
	return config
}

func validateAtomicConfigType(valueType reflect.Type) error {
	if valueType.Kind() != reflect.Struct {
		return fmt.Errorf("config.AtomicConfig requires a value-only struct, got %s", valueType)
	}
	for index := 0; index < valueType.NumField(); index++ {
		field := valueType.Field(index)
		if !field.IsExported() {
			return fmt.Errorf("config.AtomicConfig field %s.%s must be exported", valueType, field.Name)
		}
		switch field.Type.Kind() {
		case reflect.Bool,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64,
			reflect.String:
		case reflect.Struct:
			if !isAtomicConfigValueType(field.Type) {
				return fmt.Errorf("config.AtomicConfig field %s.%s is not safely copyable", valueType, field.Name)
			}
		default:
			return fmt.Errorf("config.AtomicConfig field %s.%s has unsupported type %s", valueType, field.Name, field.Type)
		}
	}
	return nil
}

func isAtomicConfigValueType(valueType reflect.Type) bool {
	switch valueType.Kind() {
	case reflect.Struct:
		for index := 0; index < valueType.NumField(); index++ {
			field := valueType.Field(index)
			if !field.IsExported() || !isAtomicConfigValueType(field.Type) {
				return false
			}
		}
		return true
	case reflect.Array:
		return isAtomicConfigValueType(valueType.Elem())
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64,
		reflect.String:
		return true
	default:
		return false
	}
}

func (config *AtomicConfig[T]) Load() T {
	if config == nil {
		var zero T
		return zero
	}
	current := config.value.Load()
	if current == nil {
		var zero T
		return zero
	}
	return *current
}

func (config *AtomicConfig[T]) store(next T) {
	snapshot := new(T)
	*snapshot = next
	config.value.Store(snapshot)
}

// snapshotForConfig lets ConfigManager serialize an immutable copy rather than
// reflecting over storage that can change concurrently.
func (config *AtomicConfig[T]) snapshotForConfig() (interface{}, error) {
	if config == nil {
		return nil, fmt.Errorf("cannot snapshot a nil AtomicConfig")
	}
	if err := validateAtomicConfigType(reflect.TypeOf((*T)(nil)).Elem()); err != nil {
		return nil, err
	}
	snapshot := config.Load()
	return &snapshot, nil
}

func (config *AtomicConfig[T]) validateConfigMap(configMap map[string]string) error {
	if config == nil {
		return fmt.Errorf("cannot validate a nil AtomicConfig")
	}
	if err := validateAtomicConfigType(reflect.TypeOf((*T)(nil)).Elem()); err != nil {
		return err
	}
	next := config.Load()
	if err := updateStructFromMap(&next, configMap, true); err != nil {
		return err
	}
	return config.validateSnapshot(next)
}

// updateFromConfigMap applies reflection updates to a private copy and then
// publishes the complete snapshot with one atomic operation. Compare-and-swap
// prevents concurrent single-field updates from overwriting each other.
func (config *AtomicConfig[T]) updateFromConfigMap(configMap map[string]string) error {
	if config == nil {
		return fmt.Errorf("cannot update a nil AtomicConfig")
	}
	if err := validateAtomicConfigType(reflect.TypeOf((*T)(nil)).Elem()); err != nil {
		return err
	}
	for {
		current := config.value.Load()
		var next T
		if current != nil {
			next = *current
		}
		if err := updateStructFromMap(&next, configMap, true); err != nil {
			return err
		}
		if err := config.validateSnapshot(next); err != nil {
			return err
		}

		snapshot := new(T)
		*snapshot = next
		if config.value.CompareAndSwap(current, snapshot) {
			return nil
		}
	}
}

func (config *AtomicConfig[T]) validateSnapshot(next T) error {
	if config.validate == nil {
		return nil
	}
	if err := config.validate(next); err != nil {
		return fmt.Errorf("invalid configuration snapshot: %w", err)
	}
	return nil
}
