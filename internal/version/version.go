package version

import (
	"bytes"
	"strconv"
	"time"
)

const (
	defaultVersion = "dev"
)

var (
	ProductVersion = defaultVersion
	CommitHash     = ""
	BuildTimestamp = ""
)

type MyTime struct {
	*time.Time
}

func (t *MyTime) MarshalJSON() ([]byte, error) {
	if t.Time.IsZero() {
		return []byte("null"), nil
	}

	return []byte("\"" + t.Time.Format(time.RFC3339) + "\""), nil
}

// UnmarshalJSON implements the json.Unmarshaler interface.
// The time is expected to be a quoted string in RFC 3339 format.
func (t *MyTime) UnmarshalJSON(data []byte) (err error) {
	// by convention, unmarshalers implement UnmarshalJSON([]byte("null")) as a no-op.
	if bytes.Equal(data, []byte("null")) {
		return nil
	}

	// Fractional seconds are handled implicitly by Parse.
	tt, err := time.Parse("\""+time.RFC3339+"\"", string(data))
	*t = MyTime{&tt}
	return
}

type VersionOutput struct {
	Version    string  `json:"version"`
	CommitHash string  `json:"commitHash,omitempty"`
	BuildTime  *MyTime `json:"buildTimestamp,omitempty"`
}

func Version() VersionOutput {
	var buildTime time.Time
	if BuildTimestamp != "" {
		if parsedTimestamp, err := strconv.ParseInt(BuildTimestamp, 10, 32); err == nil {
			buildTime = time.Unix(parsedTimestamp, 0)
		}
	}

	if ProductVersion == "" {
		ProductVersion = defaultVersion
	}

	return VersionOutput{
		Version:    ProductVersion,
		CommitHash: CommitHash,
		BuildTime:  &MyTime{&buildTime},
	}
}
