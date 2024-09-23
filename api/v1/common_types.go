package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	ctrl_client "sigs.k8s.io/controller-runtime/pkg/client"
)

// EnvVar represents an environment variable present in a Container or Executable.
// +k8s:openapi-gen=true
type EnvVar struct {
	// Name of the environment variable
	Name string `json:"name"`

	// Value of the environment variable. Defaults to "" (empty string).
	// +optional
	Value string `json:"value,omitempty"`
	// CONSIDER allowing expansion of existing variable references e.g. using ${VAR_NAME} syntax and $$ to escape the $ sign
}

const LogSubresourceName = "log"

// +kubebuilder:object:generate=false
// +k8s:openapi-gen=false
type StdIoStreamableResource interface {
	GetUID() types.UID
	NamespacedName() types.NamespacedName
	GetStdOutFile() string
	GetStdErrFile() string
	Done() bool

	// This is set by Kubernetes with 1-second precision when the resource is deleted
	// Hence we use metav1.Time here instead of metav1.MicroTime
	GetDeletionTimestamp() *metav1.Time
}

// +kubebuilder:object:generate=false
// +k8s:openapi-gen=false
type NamespacedNameWithKind struct {
	types.NamespacedName
	Kind schema.GroupVersionKind
}

func (nnk NamespacedNameWithKind) Empty() bool {
	return len(nnk.Name) == 0 && len(nnk.Namespace) == 0 && nnk.Kind.Empty()
}

func (nnk NamespacedNameWithKind) String() string {
	return nnk.NamespacedName.String() + " (" + nnk.Kind.String() + ")"
}

// +kubebuilder:object:generate=false
// +k8s:openapi-gen=false
type NamespacedNameWithKindAndUid struct {
	NamespacedNameWithKind
	Uid types.UID
}

func GetNamespacedNameWithKind(obj ctrl_client.Object) NamespacedNameWithKind {
	return NamespacedNameWithKind{
		NamespacedName: types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      obj.GetName(),
		},
		Kind: obj.GetObjectKind().GroupVersionKind(),
	}
}
