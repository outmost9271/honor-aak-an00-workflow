package bridge

import "fmt"

// BridgeError 对应 Python 版 BridgeError：带稳定错误码的保守失败。
type BridgeError struct{ Code string }

func (e *BridgeError) Error() string { return e.Code }

func errCode(code string) *BridgeError { return &BridgeError{Code: code} }

// SkipError 终止项：照片无需处理（Python Skip）。
type SkipError struct{ Code string }

func (e *SkipError) Error() string { return e.Code }

func skip(code string) *SkipError { return &SkipError{Code: code} }

// ConflictError 终止项：状态冲突，需要人工复核（Python Conflict）。
type ConflictError struct{ Code string }

func (e *ConflictError) Error() string { return e.Code }

func conflict(code string) *ConflictError { return &ConflictError{Code: code} }

// APIError 上游 HTTP 错误；MissingOrDenied 表示固定消息的“不存在或无权限”。
type APIError struct {
	Status         int
	MissingOrDenied bool
}

func (e *APIError) Error() string { return fmt.Sprintf("API_%d", e.Status) }
