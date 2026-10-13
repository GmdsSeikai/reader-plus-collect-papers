package reader

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

func DecodeAnalysisResult(body string, input AnalysisInput) (AnalysisResult, error) {
	var result AnalysisResult
	if len(body) > 1<<20 || !utf8.ValidString(body) {
		return result, errInvalidAnalysis
	}
	d := json.NewDecoder(strings.NewReader(body))
	d.UseNumber()
	if err := uniqueJSONValue(d, 0); err != nil {
		return result, fmt.Errorf("%w: %v", errInvalidAnalysis, err)
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return result, fmt.Errorf("%w: 多余 JSON 内容", errInvalidAnalysis)
	}
	top, err := requiredAnalysisObject([]byte(body), "schema_version", "title_zh", "abstract_zh", "assessments")
	if err != nil {
		return result, err
	}
	var assessments []json.RawMessage
	if err := json.Unmarshal(top["assessments"], &assessments); err != nil {
		return result, errInvalidAnalysis
	}
	for _, raw := range assessments {
		a, err := requiredAnalysisObject(raw, "task_id", "profile_hash", "relevance_score", "reason_zh", "evidence")
		if err != nil {
			return result, err
		}
		var evidence []json.RawMessage
		if err := json.Unmarshal(a["evidence"], &evidence); err != nil {
			return result, errInvalidAnalysis
		}
		for _, quote := range evidence {
			if _, err := requiredAnalysisObject(quote, "field", "quote"); err != nil {
				return result, err
			}
		}
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		return result, fmt.Errorf("%w: 字段类型无效", errInvalidAnalysis)
	}
	if err := ValidateAnalysisResult(result, input, input.Title, input.Abstract); err != nil {
		return result, err
	}
	return result, nil
}

func requiredAnalysisObject(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || len(object) != len(names) {
		return nil, fmt.Errorf("%w: 必填字段缺失或包含未知字段", errInvalidAnalysis)
	}
	for _, name := range names {
		value, ok := object[name]
		if !ok || string(value) == "null" {
			return nil, fmt.Errorf("%w: %s 缺失或为 null", errInvalidAnalysis, name)
		}
	}
	return object, nil
}

// encoding/json otherwise accepts repeated keys and silently keeps the last.
func uniqueJSONValue(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("JSON 嵌套过深")
	}
	value, err := d.Token()
	if err != nil {
		return err
	}
	mark, container := value.(json.Delim)
	if !container {
		return nil
	}
	switch mark {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("JSON 字段重复")
			}
			seen[name] = true
			if err := uniqueJSONValue(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueJSONValue(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("JSON 容器无效")
	}
	_, err = d.Token()
	return err
}
