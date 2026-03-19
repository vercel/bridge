package ptr

// OrNil returns a pointer to v, or nil if v is the zero value.
func OrNil[T comparable](v T) *T {
	var zero T
	if v == zero {
		return nil
	}
	return &v
}
