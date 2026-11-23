package gofakes3

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitEntityTags(t *testing.T) {
	for _, test := range []struct {
		name, value string
		want        []string
	}{
		{"Empty", "", nil},
		{"Whitespace", " \t", nil},
		{"Wildcard", " \t*\t ", []string{"*"}},
		{"Strong", `"etag"`, []string{`"etag"`}},
		{"Weak", `W/"etag"`, []string{`W/"etag"`}},
		{"EmptyTag", `""`, []string{`""`}},
		{"List", "\t\"one\" , W/\"two\" ", []string{`"one"`, `W/"two"`}},
		{"Comma", `"one,two", "three"`, []string{`"one,two"`, `"three"`}},
		{"Backslash", `"one\two"`, []string{`"one\two"`}},
		{"ObsText", "\"\x80\xff\"", []string{"\"\x80\xff\""}},
		{"Unquoted", "etag", []string{`"etag"`}},
		{"UnquotedMultipart", "0123456789abcdef-2", []string{`"0123456789abcdef-2"`}},
		{"UnquotedList", "one,two", nil},
		{"UnquotedSpace", "one two", nil},
		{"UnquotedControl", "one\x00two", nil},
		{"UnquotedDEL", "one\x7f", nil},
		{"UnquotedObsText", "one\xff", nil},
		{"UnquotedWildcard", "one*", nil},
		{"Unterminated", `"etag`, nil},
		{"SpaceInTag", `"one two"`, nil},
		{"Control", "\"one\x00two\"", nil},
		{"DEL", "\"one\x7ftwo\"", nil},
		{"CRLF", "\"one\"\r\n", nil},
		{"MissingComma", `"one" "two"`, nil},
		{"TrailingComma", `"one",`, nil},
		{"LeadingComma", `,"one"`, nil},
		{"WildcardList", `*, "one"`, nil},
		{"WeakWildcard", "W/*", nil},
		{"LowercaseWeak", `w/"one"`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, splitEntityTags(test.value))
		})
	}
}

func TestConditionsFromRequest(t *testing.T) {
	for _, test := range []struct {
		name        string
		header      http.Header
		match, none string
		invalid     bool
	}{
		{"Absent", http.Header{}, "", "", false},
		{"Match", http.Header{"If-Match": {`"one"`}}, `"one"`, "", false},
		{"Repeated", http.Header{"If-Match": {`"one"`, `"two"`}}, `"one","two"`, "", false},
		{"None", http.Header{"If-None-Match": {"*"}}, "", "*", false},
		{"Both", http.Header{"If-Match": {`"one"`}, "If-None-Match": {"*"}}, `"one"`, "*", false},
		{"EmptyMatch", http.Header{"If-Match": {""}}, "", "", true},
		{"EmptyNone", http.Header{"If-None-Match": {""}}, "", "", true},
		{"Unquoted", http.Header{"If-Match": {"one"}}, "one", "", false},
		{"UnquotedList", http.Header{"If-Match": {"one", "two"}}, "one,two", "", true},
		{"Unterminated", http.Header{"If-None-Match": {`"one`}}, "", `"one`, true},
		{"RepeatedWildcard", http.Header{"If-Match": {"*", "*"}}, "*,*", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, "/bucket/key", nil)
			r.Header = test.header
			got, err := ParseWriteConditions(r.Header)
			if test.invalid {
				require.Equal(t, ErrInvalidArgument, err)
				return
			}
			require.NoError(t, err)
			if test.name == "Absent" {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, splitEntityTags(test.match), got.match)
			assert.Equal(t, splitEntityTags(test.none), got.none)
		})
	}
}

func FuzzEntityTags(f *testing.F) {
	for _, value := range []string{"", "*", `""`, `"one"`, `W/"one"`, `"one,two", W/"three"`, `"unterminated`, "\"\xff\"", "\x00", "0123456789abcdef-2"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		tags := splitEntityTags(value)
		if len(tags) == 0 {
			return
		}
		assert.Equal(t, tags, splitEntityTags(strings.Join(tags, ", ")))
		assert.Empty(t, splitEntityTags(value+"\x00"))
		for _, tag := range tags {
			strong := strings.TrimPrefix(tag, "W/")
			match := WriteConditions{match: tags}
			none := WriteConditions{none: tags}
			require.Equal(t, none.Check(true, strong), ErrPreconditionFailed)
			if tag == strong {
				require.NoError(t, match.Check(true, strong))
			} else {
				weak := WriteConditions{match: []string{tag}}
				require.Equal(t, weak.Check(true, strong), ErrPreconditionFailed)
			}
		}
	})
}

func TestWriteConditionsNeedsETag(t *testing.T) {
	for _, test := range []struct {
		name   string
		header http.Header
		want   bool
	}{
		{"ExistenceOnly", http.Header{"If-Match": {"*"}, "If-None-Match": {"*"}}, false},
		{"MatchTag", http.Header{"If-Match": {`"etag"`}}, true},
		{"NoneMatchTag", http.Header{"If-None-Match": {`W/"etag"`}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			conditions, err := ParseWriteConditions(test.header)
			require.NoError(t, err)
			assert.Equal(t, test.want, conditions.NeedsETag())
		})
	}
}

func TestCheckPutConditions(t *testing.T) {
	invalid := `"unterminated`
	etag := `"etag"`
	for _, test := range []struct {
		name       string
		conditions *PutConditions
		exists     bool
		want       error
	}{
		{"Unconditional", nil, false, nil},
		{"InvalidMatch", &PutConditions{IfMatch: &invalid}, true, ErrInvalidArgument},
		{"InvalidNoneMatch", &PutConditions{IfNoneMatch: &invalid}, true, ErrInvalidArgument},
		{"MissingDestination", &PutConditions{IfMatch: &etag}, false, ErrPreconditionFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := CheckPutConditions(test.conditions, &ConditionalObjectInfo{Exists: test.exists})
			assert.Equal(t, test.want, err)
		})
	}
}

func TestCheckCopySource(t *testing.T) {
	const (
		prefix = "X-Amz-Copy-Source-"
		before = "Mon, 02 Jan 2006 15:04:04 GMT"
		at     = "Mon, 02 Jan 2006 15:04:05 GMT"
	)
	obj := &Object{Hash: []byte{0xab}, Metadata: map[string]string{"Last-Modified": at}}
	for _, test := range []struct {
		name   string
		header http.Header
		want   error
	}{
		{"InvalidMatch", http.Header{prefix + "If-Match": {`"ab`}}, ErrInvalidArgument},
		{"UnmodifiedSinceFails", http.Header{prefix + "If-Unmodified-Since": {before}}, ErrPreconditionFailed},
		{"ModifiedSinceFailsAtSameTime", http.Header{prefix + "If-Modified-Since": {at}}, ErrPreconditionFailed},
		{"ModifiedSincePasses", http.Header{prefix + "If-Modified-Since": {before}}, nil},
		{"UnmodifiedSincePassesAtSameTime", http.Header{prefix + "If-Unmodified-Since": {at}}, nil},
		{"MatchTakesPrecedence", http.Header{prefix + "If-Match": {`"wrong"`, `"ab"`}, prefix + "If-Unmodified-Since": {before}}, nil},
		{"NoneMatchTakesPrecedence", http.Header{prefix + "If-None-Match": {`"other"`}, prefix + "If-Modified-Since": {at}}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, checkCopySource(test.header, obj))
		})
	}
}
