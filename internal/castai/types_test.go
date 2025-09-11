package castai

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAction(t *testing.T) {

	t.Run("should not panic when action is not specified", func(t *testing.T) {
		action := &Action{}

		switch action.Action().(type) {
		default:
			t.Log("ok")
		}
	})

	t.Run("should return the right type when calling action", func(t *testing.T) {
		r := require.New(t)

		tests := []struct {
			action         Action
			expectedAction interface{}
		}{
			{
				action:         Action{ActionInstall: &ActionInstall{}},
				expectedAction: &ActionInstall{},
			},
			{
				action:         Action{ActionUpgrade: &ActionUpgrade{}},
				expectedAction: &ActionUpgrade{},
			},
			{
				action:         Action{ActionRollback: &ActionRollback{}},
				expectedAction: &ActionRollback{},
			},
			{
				action:         Action{ActionUninstall: &ActionUninstall{}},
				expectedAction: &ActionUninstall{},
			},
		}

		for _, tt := range tests {
			r.Equal(tt.expectedAction, tt.action.Action())
		}

	})
}
