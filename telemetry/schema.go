// Package telemetry records schema-validated usage through private OpenTelemetry providers.
package telemetry

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// Signal identifies how an observation is represented in OpenTelemetry.
type Signal uint8

const (
	Span Signal = iota + 1
	Event
	Counter
	Histogram
)

// Kind is the declared type of a privacy-safe attribute.
type Kind uint8

const (
	Enum Kind = iota + 1
	Boolean
	Integer
	Duration
	Count
	Size
	Length
	Identifier
)

// Unit identifies the unit of a numeric signal or field.
type Unit string

const (
	UnitOne          Unit = "1"
	UnitBytes        Unit = "By"
	UnitMilliseconds Unit = "ms"
)

// EnumMember is an application-declared, closed-set enum value.
type EnumMember string

// Field describes an allowed attribute. Enum members must be declared constants.
type Field struct {
	Kind     Kind
	Members  []EnumMember
	Required bool
}

// Schema defines one immutable operation and version. Duration fields use milliseconds,
// size fields use bytes, and counts and lengths are unitless.
type Schema struct {
	Name    string
	Version int
	Signal  Signal
	Unit    Unit
	Fields  map[string]Field
}

// Value is constructed with the typed attribute constructors. It is not an OTel attribute.
type Value struct {
	key  string
	kind Kind
	text string
	n    int64
}

// EnumValue supplies a closed member of the named schema field.
func EnumValue(key string, value EnumMember) Value {
	return Value{key: key, kind: Enum, text: string(value)}
}

// Bool supplies a boolean.
func Bool(key string, value bool) Value {
	var n int64
	if value {
		n = 1
	}
	return Value{key: key, kind: Boolean, n: n}
}

// Int supplies an integer.
func Int(key string, value int64) Value { return Value{key: key, kind: Integer, n: value} }

// Elapsed supplies a duration converted to whole milliseconds.
func Elapsed(key string, value time.Duration) Value {
	n := value.Milliseconds()
	if value < 0 {
		n = -1
	}
	return Value{key: key, kind: Duration, n: n}
}

// Items supplies a count.
func Items(key string, value int64) Value { return Value{key: key, kind: Count, n: value} }

// Bytes supplies a size in bytes.
func Bytes(key string, value int64) Value { return Value{key: key, kind: Size, n: value} }

// ContentLength supplies only a content length, never the content.
func ContentLength(key string, value int64) Value { return Value{key: key, kind: Length, n: value} }

// HashIdentifier supplies an identifier for derivation by the API before enqueueing.
func HashIdentifier(key, raw string) Value { return Value{key: key, kind: Identifier, text: raw} }

// Observation describes a completed operation. Started and Duration are for spans;
// Amount is for metrics, in the declared schema unit.
type Observation struct {
	Name       string
	Version    int
	Started    time.Time
	Duration   time.Duration
	Amount     int64
	Attributes []Value
}

type schemaKey struct {
	name    string
	version int
}

var (
	signalName = regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z][a-z0-9_-]*\.[a-z][a-z0-9_-]*$`)
	fieldName  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	enumName   = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)
)

func registry(tool string, schemas []Schema) (map[schemaKey]Schema, bool, error) {
	result := make(map[schemaKey]Schema, len(schemas))
	byName := make(map[string]Schema)
	var useSalt bool
	for _, schema := range schemas {
		if !signalName.MatchString(schema.Name) || len(schema.Name) > 127 ||
			!strings.HasPrefix(schema.Name, tool+".") || schema.Version < 1 ||
			schema.Signal < Span || schema.Signal > Histogram || len(schema.Fields) > 24 {
			return nil, false, errors.New("invalid telemetry schema")
		}
		if schema.Signal >= Counter {
			if schema.Unit != UnitOne && schema.Unit != UnitBytes && schema.Unit != UnitMilliseconds {
				return nil, false, errors.New("invalid metric unit")
			}
		} else if schema.Unit != "" {
			return nil, false, errors.New("unit not applicable to trace signal")
		}
		if previous, ok := byName[schema.Name]; ok && (previous.Signal != schema.Signal || previous.Unit != schema.Unit) {
			return nil, false, errors.New("inconsistent telemetry signal identity")
		}
		byName[schema.Name] = schema
		key := schemaKey{schema.Name, schema.Version}
		if _, exists := result[key]; exists {
			return nil, false, errors.New("duplicate telemetry schema")
		}
		fields, salt, err := cloneFields(schema.Fields)
		if err != nil {
			return nil, false, err
		}
		useSalt = useSalt || salt
		schema.Fields = fields
		result[key] = schema
	}
	return result, useSalt, nil
}

func cloneFields(input map[string]Field) (map[string]Field, bool, error) {
	fields := make(map[string]Field, len(input))
	var useSalt bool
	for key, field := range input {
		if !fieldName.MatchString(key) || strings.HasPrefix(key, "punt.") ||
			field.Kind < Enum || field.Kind > Identifier ||
			(field.Kind == Enum) != (len(field.Members) != 0) {
			return nil, false, errors.New("invalid telemetry field")
		}
		for _, member := range field.Members {
			if !enumName.MatchString(string(member)) {
				return nil, false, errors.New("invalid enum member")
			}
		}
		field.Members = append([]EnumMember(nil), field.Members...)
		fields[key] = field
		useSalt = useSalt || field.Kind == Identifier
	}
	return fields, useSalt, nil
}

func validate(schema Schema, input []Value, salt []byte) ([]attribute.KeyValue, bool) {
	if len(input) > len(schema.Fields) {
		return nil, false
	}
	seen := make(map[string]bool, len(input))
	attrs := make([]attribute.KeyValue, 0, len(input)+1)
	for _, v := range input {
		f, exists := schema.Fields[v.key]
		if !exists || seen[v.key] || v.kind != f.Kind {
			return nil, false
		}
		a, ok := typedAttribute(f, v, salt)
		if !ok {
			return nil, false
		}
		seen[v.key] = true
		attrs = append(attrs, a)
	}
	for key, f := range schema.Fields {
		if f.Required && !seen[key] {
			return nil, false
		}
	}
	return append(attrs, attribute.Int("punt.schema.version", schema.Version)), true
}

func typedAttribute(field Field, value Value, salt []byte) (attribute.KeyValue, bool) {
	switch field.Kind {
	case Enum:
		for _, member := range field.Members {
			if string(member) == value.text {
				return attribute.String(value.key, value.text), true
			}
		}
	case Boolean:
		return attribute.Bool(value.key, value.n != 0), true
	case Integer:
		return attribute.Int64(value.key, value.n), true
	case Duration, Count, Size, Length:
		if value.n >= 0 {
			return attribute.Int64(value.key, value.n), true
		}
	case Identifier:
		if len(salt) != 0 && len(value.text) != 0 && len(value.text) <= 65_536 {
			mac := hmac.New(sha256.New, salt)
			_, _ = mac.Write([]byte(value.text))
			return attribute.String(value.key, hex.EncodeToString(mac.Sum(nil))), true
		}
	}
	return attribute.KeyValue{}, false
}
