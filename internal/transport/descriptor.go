package transport

import (
	"maps"
	"slices"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func cloneDescriptor(descriptor v1.Descriptor) v1.Descriptor {
	descriptor.URLs = slices.Clone(descriptor.URLs)
	descriptor.Annotations = maps.Clone(descriptor.Annotations)
	descriptor.Data = slices.Clone(descriptor.Data)
	if descriptor.Platform != nil {
		platform := *descriptor.Platform
		platform.OSFeatures = slices.Clone(platform.OSFeatures)
		descriptor.Platform = &platform
	}
	return descriptor
}
