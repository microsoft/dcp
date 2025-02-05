package ctrlutil

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/microsoft/usvc-apiserver/controllers"
)

const (
	pollImmediately  = true // Don't wait before polling for the first time
	waitPollInterval = 200 * time.Millisecond
)

func WaitObjectDeleted[T controllers.ObjectStruct, PT controllers.PObjectStruct[T]](
	t *testing.T,
	ctx context.Context,
	client ctrl_client.Client,
	deleted PT,
) {
	name := ctrl_client.ObjectKeyFromObject(deleted)

	objectNotFound := func(ctx context.Context) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}

		var obj T = *new(T)
		err := client.Get(ctx, name, PT(&obj))
		if err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			} else {
				return false, err
			}
		}
		return false, nil
	}

	err := wait.PollUntilContextCancel(ctx, waitPollInterval, pollImmediately, objectNotFound)
	if err != nil {
		t.Fatalf("Object '%s' was not deleted as expected: %v", name.Name, err)
	}
}
