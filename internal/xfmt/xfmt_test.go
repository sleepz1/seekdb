package xfmt

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

type kind int

func (k kind) String() string { return "kind" + fmt.Sprint(int(k)) }

func TestSprintfMatchesFmt(t *testing.T) {
	cases := []struct {
		f string
		a []any
	}{
		{"plain", nil},
		{"%s and %d", []any{"x", 42}},
		{"%q", []any{"a\"b\n\x00é "}},
		{"%v %v %v", []any{errors.New("boom"), kind(3), true}},
		{"%s", []any{kind(7)}},
		{"%d %d %d %d", []any{int64(-9), int32(5), uint8(200), uint64(1 << 63)}},
		{"%v", []any{1.5}},
		{"%v", []any{1e21}},
		{"%v", []any{float32(0.1)}},
		{"100%% sure %s", []any{"yes"}},
		{"Unknown column '%s' in 'field list'", []any{"c"}},
		{"%q", []any{'x'}},
		{"%s", []any{[]byte("raw")}},
		{"trailing %", nil},
	}
	for _, c := range cases {
		want := fmt.Sprintf(c.f, c.a...)
		if got := Sprintf(c.f, c.a...); got != want {
			t.Errorf("%q %v: got %q want %q", c.f, c.a, got, want)
		}
	}
}

func TestErrorfWraps(t *testing.T) {
	e := Errorf("recovery: %w", io.ErrUnexpectedEOF)
	if !errors.Is(e, io.ErrUnexpectedEOF) || e.Error() != "recovery: unexpected EOF" {
		t.Fatalf("got %v", e)
	}
	if errors.Unwrap(Errorf("x %d", 1)) != nil {
		t.Fatal("plain error unwraps")
	}
	if Errorf("at %d: %w", 3, io.EOF).Error() != fmt.Errorf("at %d: %w", 3, io.EOF).Error() {
		t.Fatal("message differs")
	}
}
