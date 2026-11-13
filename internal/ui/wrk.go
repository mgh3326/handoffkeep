package ui

import (
	"errors"
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/wrkrecords"
)

func uiWrkError(w http.ResponseWriter, err error) bool {
	var denied wrkrecords.Error
	if !errors.As(err, &denied) {
		return false
	}
	writeJSONError(w, wrkrecords.Status(denied), wrkrecords.Code(denied))
	return true
}
