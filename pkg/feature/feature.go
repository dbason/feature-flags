package feature

type FeatureFlag interface {
	Name() string
	Description() string
	IsEnabled() bool
}

func NewFeature(name string, description string, enabled bool) feature {
	return feature{
		name:        name,
		description: description,
		enabled:     enabled,
	}
}

// feature is the local implementation of FeatureFlag
type feature struct {
	name        string
	description string
	enabled     bool
}

func (f *feature) Name() string {
	return f.name
}

func (f *feature) Description() string {
	return f.description
}

func (f *feature) IsEnabled() bool {
	return f.enabled
}
