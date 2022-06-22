package httputil

import (
	"encoding/json"
	"mime"
	"net/http"
	"strings"
)

var jsonSetEscapeHtml = true

type errorPayload struct {
	Data  interface{} `json:"data"`
	Error string      `json:"error"`
}

// JsonSetEscapeHtml sets HTML escaping mode for WriteObj and other variants.
// This will turn json.Encoder HTML escaping on or off in this package.
func JsonSetEscapeHtml(on bool) {
	jsonSetEscapeHtml = on
}

// WriteObj200 writes an object with WriteObj and sends 200 code.
func WriteObj200(writer http.ResponseWriter, obj interface{}) (err error) {
	return WriteObj200HtmlEscape(writer, obj, jsonSetEscapeHtml)
}

// WriteObj writes an object to the response writer as JSON.
// It also sets Content-Type to application/json and send in HTTP `code`.
// The `code` should be 2xx.
func WriteObj(writer http.ResponseWriter, obj interface{}, code int) (err error) {
	return WriteObjHtmlEscape(writer, obj, code, jsonSetEscapeHtml)
}

// WriteError writes a string of error as JSON.
// See also WriteErrorObj.
func WriteError(writer http.ResponseWriter, err error, code int) {
	WriteErrorHtmlEscape(writer, err, code, jsonSetEscapeHtml)
}

// WriteErrorObj writes obj as response and send in HTTP error code.
// This is a helper function to marshal obj as JSON and send it together with error.
// Error will always be written in `error` JSON field.
func WriteErrorObj(writer http.ResponseWriter, obj interface{}, err error, code int) {
	WriteErrorObjHtmlEscape(writer, obj, err, code, jsonSetEscapeHtml)
}

// WriteObj200HtmlEscape writes an object with WriteObj and sends 200 code.
// It has option to override JSON encoding HTML escape.
func WriteObj200HtmlEscape(writer http.ResponseWriter, obj interface{}, htmlEscape bool) (err error) {
	return WriteObjHtmlEscape(writer, obj, http.StatusOK, htmlEscape)
}

// WriteObjHtmlEscape writes an object to the response writer as JSON.
// It also sets Content-Type to application/json and send in HTTP `code`.
// The `code` should be 2xx.
//
// It has option to override JSON encoding HTML escape.
func WriteObjHtmlEscape(writer http.ResponseWriter, obj interface{}, code int, htmlEscape bool) (err error) {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(htmlEscape)
	writer.Header().Add("Content-Type", "application/json")
	writer.WriteHeader(code)
	return encoder.Encode(obj)
}

// WriteErrorHtmlEscape writes a string of error as JSON.
// See also WriteErrorObj.
func WriteErrorHtmlEscape(writer http.ResponseWriter, err error, code int, htmlEscape bool) {
	payload := map[string]string{}
	payload["error"] = err.Error()
	_ = WriteObjHtmlEscape(writer, payload, code, htmlEscape)
}

// WriteErrorObjHtmlEscape writes obj as response and send in HTTP error code.
// This is a helper function to marshal obj as JSON and send it together with error.
// Error will always be written in `error` JSON field.
func WriteErrorObjHtmlEscape(writer http.ResponseWriter, obj interface{}, err error, code int, htmlEscape bool) {
	payload := &errorPayload{
		Data:  obj,
		Error: err.Error(),
	}

	_ = WriteObjHtmlEscape(writer, payload, code, htmlEscape)
}

// HasContentType determines whether the request `content-type` includes a
// server-acceptable mime-type. Do not just compare Content-Type with == operator as the value may contain additional
// texts.
//
// Failure should yield an HTTP 415 (`http.StatusUnsupportedMediaType`)
func HasContentType(r *http.Request, mimetype string) bool {
	contentType := r.Header.Get("Content-type")
	if contentType == "" {
		return mimetype == "application/octet-stream"
	}

	for _, v := range strings.Split(contentType, ",") {
		t, _, err := mime.ParseMediaType(v)
		if err != nil {
			break
		}
		if t == mimetype {
			return true
		}
	}
	return false
}
