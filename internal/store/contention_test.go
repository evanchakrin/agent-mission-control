package store

import (
	"errors"
	"fmt"
	"testing"
)

type testSQLiteCode int

func (e testSQLiteCode) Error() string { return "fixture" }
func (e testSQLiteCode) Code() int     { return int(e) }
func TestContentionClassificationIsCodeBased(t *testing.T) {
	for _, code := range []int{5, 6, 261, 517, 262} {
		if !IsContention(fmt.Errorf("wrapped: %w", testSQLiteCode(code))) {
			t.Fatal(code)
		}
	}
	for _, err := range []error{nil, errors.New("database is locked"), testSQLiteCode(11), testSQLiteCode(13)} {
		if IsContention(err) {
			t.Fatal("non-contention swallowed", err)
		}
	}
}
