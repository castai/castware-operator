package castai

type ApiError struct {
	Message         string        `json:"message"`
	FieldViolations []interface{} `json:"fieldViolations"`
}

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type Component struct {
	Id           string   `json:"id"`
	Name         string   `json:"name"`
	HelmChart    string   `json:"helm_chart"`
	Dependencies []string `json:"dependencies"`
}
