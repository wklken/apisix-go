package cache

import "golang.org/x/sync/singleflight"

type Retrieve[V any] func(key string) (V, error)

type Retriever[V any] struct {
	g        singleflight.Group
	retrieve Retrieve[V]
}

func NewRetriever[V any](retrieve Retrieve[V]) *Retriever[V] {
	return &Retriever[V]{
		retrieve: retrieve,
	}
}

func (r *Retriever[V]) Get(key string) (V, error) {
	v, err, _ := r.g.Do(key, func() (interface{}, error) {
		return r.retrieve(key)
	})
	if err != nil {
		return *new(V), err
	}
	return v.(V), nil
}
