package test

import (
	"github.com/google/uuid"

	"github.com/castai/castware-operator/internal/castai"
)

func CreateUserObject() castai.User {
	return castai.User{
		ID:       uuid.NewString(),
		Username: "test-user",
	}
}

func CreateComponentObject() castai.Component {
	return castai.Component{
		Id:        uuid.NewString(),
		Name:      "test-component",
		HelmChart: "test-helm-chart",
	}
}
