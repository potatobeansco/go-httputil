package httputil_test

import (
	"encoding/json"
	"errors"
	"git.padmadigital.id/potato/go-httputil"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWriteObj(t *testing.T) {
	rec := httptest.NewRecorder()
	type testData struct {
		TestField1 string `json:"test_field_1"`
		TestField2 int64  `json:"test_field_2"`
	}

	td := testData{
		TestField1: time.Now().Format(time.ANSIC),
		TestField2: time.Now().UnixNano(),
	}

	err := httputil.WriteObj(rec, td, http.StatusOK)
	if err != nil {
		t.Fatal(err)
	}

	if rec.Result().Header.Get("Content-Type") != "application/json" {
		t.Fatal("Content-Type is not application/json")
	}

	body, err := ioutil.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal("cannot read response body")
	}

	expect, _ := json.Marshal(td)
	expect = append(expect, '\n')
	if string(body) != string(expect) {
		t.Fatalf("%s (len %d) is not equal to %s (len %d)", body, len(body), expect, len(expect))
	}
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	type testError struct {
		Error error `json:"error"`
	}

	te := testError{Error: errors.New(time.Now().Format(time.ANSIC))}

	httputil.WriteError(rec, te.Error, http.StatusInternalServerError)
}

func TestWriteErrorObj(t *testing.T) {
	rec := httptest.NewRecorder()
	type testData struct {
		TestField1 string `json:"test_field_1"`
		TestField2 int64  `json:"test_field_2"`
	}

	type testError struct {
		Data  *testData `json:"data"`
		Error string    `json:"error"`
	}

	td := testData{
		TestField1: time.Now().Format(time.ANSIC),
		TestField2: time.Now().UnixNano(),
	}

	dummyErr := errors.New(time.Now().Format(time.ANSIC))
	te := testError{
		Data:  &td,
		Error: dummyErr.Error(),
	}

	httputil.WriteErrorObj(rec, td, dummyErr, http.StatusInternalServerError)

	if rec.Result().Header.Get("Content-Type") != "application/json" {
		t.Fatal("Content-Type is not application/json")
	}

	body, err := ioutil.ReadAll(rec.Result().Body)
	if err != nil {
		t.Fatal("cannot read response body")
	}

	expect, _ := json.Marshal(te)
	expect = append(expect, '\n')
	if string(body) != string(expect) {
		t.Fatalf("%s (len %d) is not equal to %s (len %d)", body, len(body), expect, len(expect))
	}
}
