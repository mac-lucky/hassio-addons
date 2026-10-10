package secret

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const sentinel = "s3ntinel-VALUE-do-not-print"

func TestValueNeverFormats(t *testing.T) {
	v := New(sentinel)
	wrapped := struct {
		V Value
		P *Value
	}{v, &v}

	outputs := []string{
		v.String(),
		fmt.Sprint(v),
		fmt.Sprintf("%v %+v %#v %s %q %x %X %d", v, v, v, v, v, v, v, v),
		fmt.Sprintf("%v %+v %#v", wrapped, wrapped, wrapped),
		fmt.Sprintf("%v", []Value{v}),
		fmt.Sprintf("%v", map[string]Value{"k": v}),
	}
	js, err := json.Marshal(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, string(js))

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("msg", "v", v, "wrapped", wrapped)
	logger.With("v", v).Info("again")
	jsonLog := slog.New(slog.NewJSONHandler(&buf, nil))
	jsonLog.Info("msg", "v", v, "wrapped", wrapped)
	outputs = append(outputs, buf.String())

	for _, out := range outputs {
		if strings.Contains(out, sentinel) || strings.Contains(out, fmt.Sprintf("%x", sentinel)) {
			t.Fatalf("value leaked: %s", out)
		}
	}
	if v.Reveal() != sentinel {
		t.Fatal("Reveal lost the value")
	}
}

func TestFingerprint(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	a, b := New("one"), New("two")
	if a.Fingerprint(key) == b.Fingerprint(key) {
		t.Fatal("different values share a fingerprint")
	}
	if a.Fingerprint(key) != New("one").Fingerprint(key) {
		t.Fatal("fingerprint is not stable")
	}
	if a.Fingerprint(key) == a.Fingerprint([]byte("another-key-another-key-another!")) {
		t.Fatal("fingerprint ignores the key")
	}
	if !a.Equal(New("one")) || a.Equal(b) {
		t.Fatal("Equal is wrong")
	}
}
