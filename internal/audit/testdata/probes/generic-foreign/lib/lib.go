package lib

// A generic function and a generic type with methods, both declared here so
// that the package under test instantiates a generic it does not declare. The
// body of each is in this package's archive, and the importer has to read it.
func Max[T int | string](a, b T) T {
	if a > b {
		return a
	}
	return b
}

type Box[T any] struct{ v T }

func (b *Box[T]) Set(x T) { b.v = x }

func (b *Box[T]) Get() T { return b.v }
