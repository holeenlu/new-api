package common

import (
	"errors"
	"regexp"
)

const (
	paramOverrideContextRequestHeaders = "request_headers"
	paramOverrideContextHeaderOverride = "header_override"
	paramOverrideContextAuditRecorder  = "__param_override_audit_recorder"
)

var negativeIndexRegexp = regexp.MustCompile(`\.(-\d+)`)

var errSourceHeaderNotFound = errors.New("source header does not exist")

var paramOverrideSensitivePathPrefixes = []string{
	"model",
	"original_model",
	"upstream_model",
	"service_tier",
	"inference_geo",
	"speed",
	"messages",
	"input",
	"instructions",
	"system",
	"contents",
	"systemInstruction",
	"system_instruction",
}
