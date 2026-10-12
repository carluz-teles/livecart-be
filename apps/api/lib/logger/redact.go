package logger

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const redacted = "[redacted]"

var (
	checkoutPath = regexp.MustCompile(`(?i)(/(?:checkout|cart)/)[^/?#\s"'<>\\]+`)
	tokenPair    = regexp.MustCompile(`(?i)((?:cart[_-]?token|checkout[_-]?token|card[_-]?token|` +
		`access[_-]?token|refresh[_-]?token|token|api[_-]?key)\s*["']?\s*[:=]\s*)` +
		`(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s&,;}"']+)`)
	keySeparators = strings.NewReplacer("_", "", "-", "", ".", "")
)

// redactingCore protects application logs, including errors and fields bound
// before a log call. IDs remain available for correlation; bearer checkout
// URLs, credentials and customer/payment payloads must not become log data.
type redactingCore struct {
	zapcore.Core
	sensitiveNamespace bool
}

func (c *redactingCore) With(fields []zapcore.Field) zapcore.Core {
	fields, sensitive := redactFields(fields, c.sensitiveNamespace)
	return &redactingCore{Core: c.Core.With(fields), sensitiveNamespace: sensitive}
}

func (c *redactingCore) Check(entry zapcore.Entry, checked *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(entry.Level) {
		return checked.AddCore(entry, c)
	}
	return checked
}

func (c *redactingCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	entry.Message = redactText(entry.Message)
	fields, _ = redactFields(fields, c.sensitiveNamespace)
	return c.Core.Write(entry, fields)
}

func sensitiveKey(key string) bool {
	key = strings.ToLower(keySeparators.Replace(key))
	switch key {
	case "token", "carttoken", "checkouttoken", "cardtoken", "accesstoken", "refreshtoken",
		"authorization", "password", "secret", "apikey", "apisecret",
		"requestpayload", "responsebody", "requestbody", "body",
		"customer", "payer", "card", "creditcard", "billingaddress", "address",
		"email", "phone", "phones", "document", "cpf", "cnpj", "cvv", "cardnumber":
		return true
	default:
		return false
	}
}

func redactText(value string) string {
	// Raw provider payloads can also arrive embedded in wrapped error strings.
	// Keep the operation/status prefix, never serialize that payload again.
	if start := strings.IndexByte(value, '{'); start >= 0 {
		value = value[:start] + redacted
	}
	value = checkoutPath.ReplaceAllString(value, "${1}"+redacted)
	return tokenPair.ReplaceAllString(value, "${1}"+redacted)
}

func redactFields(fields []zapcore.Field, sensitiveNamespace bool) ([]zapcore.Field, bool) {
	out := make([]zapcore.Field, 0, len(fields))
	for _, field := range fields {
		if field.Type == zapcore.NamespaceType {
			sensitiveNamespace = sensitiveNamespace || sensitiveKey(field.Key)
			out = append(out, field)
			continue
		}
		if sensitiveNamespace || sensitiveKey(field.Key) {
			out = append(out, zap.String(field.Key, redacted))
			continue
		}
		if field.Type == zapcore.ErrorType {
			var safe interface{ LogMessage() string }
			if errors.As(field.Interface.(error), &safe) {
				out = append(out, zap.String(field.Key, redactText(safe.LogMessage())))
				continue
			}
		}
		switch field.Type {
		case zapcore.StringType:
			field.String = redactText(field.String)
		case zapcore.ByteStringType:
			field = zap.String(field.Key, redactText(string(field.Interface.([]byte))))
		case zapcore.ErrorType, zapcore.StringerType, zapcore.ReflectType,
			zapcore.ObjectMarshalerType, zapcore.ArrayMarshalerType, zapcore.InlineMarshalerType:
			// Let Zap encode errorVerbose, objects and arrays first, then scrub
			// all nested keys. Copy via JSON so caller-owned maps are untouched.
			encoder := zapcore.NewMapObjectEncoder()
			field.AddTo(encoder)
			encoded, err := json.Marshal(encoder.Fields)
			if err != nil {
				out = append(out, zap.String(field.Key, redacted))
				continue
			}
			var values map[string]any
			decoder := json.NewDecoder(bytes.NewReader(encoded))
			decoder.UseNumber()
			if err := decoder.Decode(&values); err != nil {
				out = append(out, zap.String(field.Key, redacted))
				continue
			}
			keys := make([]string, 0, len(values))
			for key := range values {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				value := redactValue(key, values[key])
				// json.Number also implements Stringer; Any would turn an
				// inline numeric field into a JSON string.
				out = append(out, zap.Reflect(key, value))
			}
			continue
		}
		out = append(out, field)
	}
	return out, sensitiveNamespace
}

func redactValue(key string, value any) any {
	if sensitiveKey(key) {
		return redacted
	}
	switch value := value.(type) {
	case string:
		return redactText(value)
	case map[string]any:
		for key, item := range value {
			value[key] = redactValue(key, item)
		}
		return value
	case []any:
		for i, item := range value {
			value[i] = redactValue("", item)
		}
		return value
	default:
		return value
	}
}
