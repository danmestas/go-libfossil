package libfossil

import (
	"time"

	"github.com/danmestas/go-libfossil/internal/fsltype"
)

// TimeToJulian converts a time.Time to a Fossil Julian day number.
func TimeToJulian(t time.Time) float64 {
	return fsltype.TimeToJulian(t)
}

// JulianToTime converts a Fossil Julian day number to time.Time.
func JulianToTime(j float64) time.Time {
	return fsltype.JulianToTime(j)
}
