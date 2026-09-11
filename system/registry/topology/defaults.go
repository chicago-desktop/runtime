package topology

import "github.com/wippyai/runtime/api/registry"

func RegistryDependencyPatterns() []registry.DependencyPattern {
	return []registry.DependencyPattern{
		{Path: "meta.parent", Description: "Reference to parent component in metadata"},
		{Path: "meta.depends_on", Description: "Explicit dependencies in metadata", AllowWildcard: true},
		{Path: "meta.groups", Description: "Group membership list in metadata", AllowWildcard: true},
		{Path: "data.config", Description: "Reference to a configuration entry"},
		{Path: "data.groups", Description: "Group membership list in data", AllowWildcard: true},
		{Path: "data.imports.*", Description: "Imported components (values only)", AllowWildcard: true},
		{Path: "data.*.depends_on", Description: "Explicit dependencies in nested structures", AllowWildcard: true},
	}
}

func LifecycleDependencyPatterns() []registry.DependencyPattern {
	return []registry.DependencyPattern{
		{Path: "data.lifecycle.requires", Description: "Lifecycle requirements", AllowWildcard: true},
		{Path: "data.lifecycle.depends_on", Description: "Legacy lifecycle dependencies", AllowWildcard: true},
	}
}

func NewDefaultResolver() (*Resolver, error) {
	resolver := NewResolver()
	for _, patterns := range [][]registry.DependencyPattern{RegistryDependencyPatterns(), LifecycleDependencyPatterns()} {
		for _, pattern := range patterns {
			if err := resolver.RegisterPattern(pattern); err != nil {
				return nil, err
			}
		}
	}
	return resolver, nil
}
