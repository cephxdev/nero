package meta

type Type uint

const (
	TypeGeneric Type = iota
	TypeAnime
)

type Metadata interface {
	Type() Type
}
