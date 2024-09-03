package v1

import (
	"context"
	"fmt"
	"slices"

	"github.com/microsoft/usvc-apiserver/pkg/commonapi"
	apiserver "github.com/tilt-dev/tilt-apiserver/pkg/server/apiserver"
	apiserver_resource "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource"
	apiserver_resourcerest "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcerest"
	apiserver_resourcestrategy "github.com/tilt-dev/tilt-apiserver/pkg/server/builder/resource/resourcestrategy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	generic_registry "k8s.io/apiserver/pkg/registry/generic"
	registry_rest "k8s.io/apiserver/pkg/registry/rest"
	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
)

// ContainerExecSpec defines an exec command to run against a Container resource
// +k8s:openapi-gen=true
type ContainerExecSpec struct {
	// The name of the Container resource to connect to
	ContainerName string `json:"containerName"`

	// Environment settings
	Env []EnvVar `json:"env,omitempty"`

	// Environment files to use to populate the environment for the command
	EnvFiles []string `json:"envFiles,omitempty"`

	// Working directory in the container for the command
	WorkingDirectory string `json:"workingDirectory,omitempty"`

	// Command to run in the container
	Command string `json:"command"`

	// Arguments to pass to the command
	Args []string `json:"args,omitempty"`
}

// ContainerExecStatus describes the status of a ContainerExec command
// +k8s:openapi-gen=true
type ContainerExecStatus struct {
	// The current state of the command execution
	State ExecutableState `json:"state,omitempty"`

	// Time the command was started
	StartupTimestamp metav1.Time `json:"startTimestamp,omitempty"`

	// Time the command finished running
	FinishTimestamp metav1.Time `json:"finishTimestamp,omitempty"`

	// Exit code of the command
	ExitCode *int32 `json:"exitCode,omitempty"`

	// The path of a temporary file that contains captured standard output data from the command
	StdOutFile string `json:"stdOutFile,omitempty"`

	// The path of a temporary file that contains captured standard error data from the command
	StdErrFile string `json:"stdErrFile,omitempty"`

	// Effective values of environment variables, after all substitutions have been applied
	// +listType:=map
	// +listMapKey:=name
	EffectiveEnv []EnvVar `json:"effectiveEnv,omitempty"`

	// Effective values of arguments to be passed to the command, after all substitutions have been applied
	// +listType:=atomic
	EffectiveArgs []string `json:"effectiveArgs,omitempty"`
}

func (ces ContainerExecStatus) CopyTo(dest apiserver_resource.ObjectWithStatusSubResource) {
	ces.DeepCopyInto(&dest.(*ContainerExec).Status)
}

// ContainerExec represents an exec command to run against a Container resource
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +k8s:openapi-gen=true
// +kubebuilder:resource:scope=Cluster
type ContainerExec struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ContainerExecSpec   `json:"spec,omitempty"`
	Status ContainerExecStatus `json:"status,omitempty"`
}

// Done implements StdOutStreamableResource.
func (ce *ContainerExec) Done() bool {
	// When the ContainerExec has a FinishTimestamp set, it is considered done no matter what other data says.
	return !ce.Status.FinishTimestamp.IsZero()
}

// StdOutFile implements StdOutStreamableResource.
func (ce *ContainerExec) GetStdOutFile() string {
	return ce.Status.StdOutFile
}

// StdErrFile implements StdOutStreamableResource.
func (ce *ContainerExec) GetStdErrFile() string {
	return ce.Status.StdErrFile
}

// GetGroupVersionResource implements resource.Object.
func (ce *ContainerExec) GetGroupVersionResource() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group:    GroupVersion.Group,
		Version:  GroupVersion.Version,
		Resource: "containerexecs",
	}
}

func (ce *ContainerExec) GetObjectMeta() *metav1.ObjectMeta {
	return &ce.ObjectMeta
}

func (ce *ContainerExec) GetStatus() apiserver_resource.StatusSubResource {
	return ce.Status
}

func (ce *ContainerExec) New() runtime.Object {
	return &ContainerExec{}
}

func (ce *ContainerExec) NewList() runtime.Object {
	return &ContainerExecList{}
}

func (ce *ContainerExec) IsStorageVersion() bool {
	return true
}

func (ce *ContainerExec) NamespaceScoped() bool {
	return false
}

func (ce *ContainerExec) ShortNames() []string {
	return []string{"ctrexec"}
}

func (ce *ContainerExec) NamespacedName() types.NamespacedName {
	return types.NamespacedName{
		Name:      ce.Name,
		Namespace: ce.Namespace,
	}
}

func (ce *ContainerExec) Validate(ctx context.Context) field.ErrorList {
	errorList := field.ErrorList{}

	if ce.Spec.Command == "" {
		errorList = append(errorList, field.Required(field.NewPath("spec", "command"), "command must be set to a non-empty value"))
	}

	if ce.Spec.ContainerName == "" {
		errorList = append(errorList, field.Required(field.NewPath("spec", "containerName"), "containerName must be set to a non-empty value"))
	}

	return errorList
}

func (ce *ContainerExec) ValidateUpdate(ctx context.Context, obj runtime.Object) field.ErrorList {
	errorList := field.ErrorList{}

	oldExec := obj.(*ContainerExec)

	if ce.Spec.Command != oldExec.Spec.Command {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "command"), "command cannot be changed"))
	}

	if ce.Spec.ContainerName != oldExec.Spec.ContainerName {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "containerName"), "containerName cannot be changed"))
	}

	if !slices.Equal(ce.Spec.Args, oldExec.Spec.Args) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "args"), "args cannot be changed"))
	}

	if !slices.Equal(ce.Spec.Env, oldExec.Spec.Env) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "env"), "env cannot be changed"))
	}

	if !slices.Equal(ce.Spec.EnvFiles, oldExec.Spec.EnvFiles) {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "envfiles"), "envfiles cannot be changed"))
	}

	if ce.Spec.WorkingDirectory != oldExec.Spec.WorkingDirectory {
		errorList = append(errorList, field.Forbidden(field.NewPath("spec", "workingdirectory"), "workingdirectory cannot be changed"))
	}

	return errorList
}

func (*ContainerExec) GenericSubResources() []apiserver_resource.GenericSubResource {
	return []apiserver_resource.GenericSubResource{
		&ContainerExecLogResource{},
	}
}

// ContainerExecList contains a list of ContainerExec instances
// +k8s:openapi-gen=true
// +kubebuilder:object:root=true
type ContainerExecList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ContainerExec `json:"items"`
}

func (cel *ContainerExecList) GetListMeta() *metav1.ListMeta {
	return &cel.ListMeta
}

func (cel *ContainerExecList) ItemCount() uint32 {
	return uint32(len(cel.Items))
}

func (cel *ContainerExecList) GetItems() []ctrl_client.Object {
	retval := make([]ctrl_client.Object, len(cel.Items))
	for i := range cel.Items {
		retval[i] = &cel.Items[i]
	}
	return retval
}

type ContainerExecLogResource struct{}

func (celr *ContainerExecLogResource) Name() string {
	return LogSubresourceName
}

func (celr *ContainerExecLogResource) GetStorageProvider(
	obj apiserver_resource.Object,
	rootPath string,
	parentSP apiserver.StorageProvider,
) apiserver.StorageProvider {
	return func(scheme *runtime.Scheme, reg generic_registry.RESTOptionsGetter) (registry_rest.Storage, error) {
		storage, err := parentSP(scheme, reg)
		if err != nil {
			return nil, fmt.Errorf("failed to get parent (%s) storage: %w", obj.GetObjectKind().GroupVersionKind().Kind, err)
		}

		exeStorage, isGetter := storage.(registry_rest.StandardStorage)
		if !isGetter {
			return nil, fmt.Errorf("parent (%s) should implement registry_rest.Getter", obj.GetObjectKind().GroupVersionKind().Kind)
		}

		logStreamFactory, found := ResourceLogStreamers.Load(obj.GetGroupVersionResource())
		if !found {
			return nil, fmt.Errorf("log stream factory not found for resource %s", obj.GetGroupVersionResource().String())
		}

		logStorage, err := NewLogStorage(exeStorage, logStreamFactory)
		if err != nil {
			return nil, err
		}

		return logStorage, nil
	}
}

func init() {
	SchemeBuilder.Register(&ContainerExec{}, &ContainerExecList{})
	SetCleanupPriority(&ContainerExec{}, 100)
}

// Ensure types support interfaces expected by our API server
var _ apiserver_resource.Object = (*ContainerExec)(nil)
var _ apiserver_resource.ObjectList = (*ContainerExecList)(nil)
var _ commonapi.ListWithObjectItems = (*ContainerExecList)(nil)
var _ apiserver_resource.ObjectWithStatusSubResource = (*ContainerExec)(nil)
var _ apiserver_resource.StatusSubResource = (*ContainerExecStatus)(nil)
var _ apiserver_resourcerest.ShortNamesProvider = (*ContainerExec)(nil)
var _ apiserver_resourcestrategy.Validater = (*ContainerExec)(nil)
var _ apiserver_resourcestrategy.ValidateUpdater = (*ContainerExec)(nil)
var _ apiserver_resource.ObjectWithGenericSubResource = (*ContainerExec)(nil)
var _ apiserver_resource.GenericSubResource = (*ContainerExecLogResource)(nil)
var _ StdIoStreamableResource = (*ContainerExec)(nil)
