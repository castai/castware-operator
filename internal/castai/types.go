package castai

type ApiError struct {
	Message         string        `json:"message"`
	FieldViolations []interface{} `json:"fieldViolations"`
}
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}
