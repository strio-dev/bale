package transport

import (
	"reflect"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestCloneDescriptorOwnsMutableFields(t *testing.T) {
	original := v1.Descriptor{
		MediaType: "application/example", Size: 12, ArtifactType: "application/example.artifact",
		URLs: []string{"https://example.com/blob"}, Annotations: map[string]string{"key": "original"},
		Data: []byte("original"), Platform: &v1.Platform{Architecture: "amd64", OS: "linux", OSFeatures: []string{"original"}},
	}
	copied := cloneDescriptor(original)
	if !reflect.DeepEqual(original, copied) {
		t.Fatal("copy changed descriptor fields")
	}
	copied.URLs[0] = "changed"
	copied.Annotations["key"] = "changed"
	copied.Data[0] = '!'
	copied.Platform.OS = "changed"
	copied.Platform.OSFeatures[0] = "changed"
	if original.URLs[0] != "https://example.com/blob" || original.Annotations["key"] != "original" || string(original.Data) != "original" || original.Platform.OS != "linux" || original.Platform.OSFeatures[0] != "original" {
		t.Fatal("copy aliases source descriptor")
	}
}

func TestCloneDescriptorPreservesNilAndEmptyFields(t *testing.T) {
	for _, original := range []v1.Descriptor{
		{},
		{URLs: []string{}, Annotations: map[string]string{}, Data: []byte{}, Platform: &v1.Platform{OSFeatures: []string{}}},
		{Platform: &v1.Platform{}},
	} {
		if copied := cloneDescriptor(original); !reflect.DeepEqual(original, copied) {
			t.Fatalf("nil or empty fields changed: %#v -> %#v", original, copied)
		}
	}
}
