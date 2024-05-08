package repo

import (
	"bufio"
	"encoding/json"
	"github.com/cephxdev/nero/internal/errors"
	"github.com/cephxdev/nero/repo/media"
	"github.com/cephxdev/nero/repo/media/meta"
	"github.com/gabriel-vasile/mimetype"
	"github.com/google/uuid"
	"go.uber.org/multierr"
	"go.uber.org/zap"
	"golang.org/x/exp/maps"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
)

type Repository struct {
	id, path, lockPath string
	logger             *zap.Logger

	items map[uuid.UUID]*media.Media
	mu    sync.RWMutex
}

func NewMemory(id string, logger *zap.Logger) *Repository {
	return &Repository{
		id:     id,
		logger: logger,
	}
}

func NewFile(id, path, lockPath string, logger *zap.Logger) (*Repository, error) {
	var err error

	if !filepath.IsAbs(path) {
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, errors.Wrap(err, "failed to make repository path absolute")
		}
	}

	if err = os.MkdirAll(path, 0); err != nil {
		return nil, errors.Wrap(err, "failed to make repository directories")
	}

	var items map[uuid.UUID]*media.Media
	if _, err := os.Stat(lockPath); err == nil {
		f, err := os.Open(lockPath)
		if err != nil {
			return nil, errors.Wrap(err, "failed to open index file")
		}
		defer func() {
			if err0 := f.Close(); err0 != nil {
				err = multierr.Append(err, errors.Wrap(err0, "failed to close index file"))
			}
		}()

		items = make(map[uuid.UUID]*media.Media)

		s := bufio.NewScanner(f)
		for s.Scan() {
			if s.Text() == "" {
				continue // skip empty lines
			}

			var m media.Media
			if err := json.Unmarshal(s.Bytes(), &m); err != nil {
				return nil, errors.Wrap(err, "failed to read index file item")
			}

			if _, ok := items[m.ID]; ok {
				logger.Warn(
					"duplicate item in index",
					zap.String("repo", id),
					zap.String("id", m.ID.String()),
				)
				continue
			}

			absPath := m.Path
			if !filepath.IsAbs(absPath) {
				absPath = filepath.Join(path, m.Path)
			}

			if _, err := os.Stat(absPath); errors.Is(err, os.ErrNotExist) {
				logger.Warn(
					"missing item in index",
					zap.String("repo", id),
					zap.String("id", m.ID.String()),
				)
				continue
			}

			items[m.ID] = &media.Media{
				ID:     m.ID,
				Format: m.Format,
				Path:   absPath,
				Meta:   m.Meta,
			}
		}

		if err := s.Err(); err != nil {
			return nil, errors.Wrap(err, "failed to read index file")
		}
	}

	return &Repository{
		id:       id,
		path:     path,
		lockPath: lockPath,
		logger:   logger,
		items:    items,
	}, err
}

func (r *Repository) ID() string {
	return r.id
}

func (r *Repository) Path() string {
	return r.path
}

func (r *Repository) LockPath() string {
	return r.lockPath
}

func (r *Repository) Get(id uuid.UUID) *media.Media {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.items == nil {
		return nil
	}
	return r.items[id]
}

func (r *Repository) Random(n int) []*media.Media {
	if n <= 0 {
		return nil
	}

	v := r.Items()
	rand.Shuffle(len(v), func(i, j int) {
		v[i], v[j] = v[j], v[i]
	})

	if len(v) > n {
		v = v[:n]
	}
	return v
}

func (r *Repository) Create(b []byte, m meta.Metadata) (*media.Media, error) {
	if r.path == "" {
		return nil, errors.ErrUnsupported
	}

	var (
		err error

		id   = uuid.New()
		mime = mimetype.Detect(b)
		path = filepath.Join(r.path, id.String()+mime.Extension())
	)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open file")
	}
	defer func() {
		if err0 := f.Close(); err0 != nil {
			err = multierr.Append(err, errors.Wrap(err0, "failed to close file"))
		}
	}()

	if _, err = f.Write(b); err != nil {
		return nil, errors.Wrap(err, "failed to write file")
	}

	m0 := &media.Media{
		ID:     id,
		Format: media.FormatUnknown,
		Path:   path,
		Meta:   m,
	}
	switch mime.String() {
	case "image/jpeg", "image/png":
		m0.Format = media.FormatImage
	case "image/vnd.mozilla.apng", "image/gif", "image/webp":
		m0.Format = media.FormatAnimatedImage
	}

	err = r.Add(m0)
	return m0, err
}

func (r *Repository) Add(m *media.Media) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.items == nil {
		r.items = make(map[uuid.UUID]*media.Media, 1)
	} else if _, ok := r.items[m.ID]; ok {
		return &ErrDuplicateID{
			ID:   m.ID.String(),
			Repo: r.id,
		}
	}

	r.items[m.ID] = m
	return r.saveSingle(m)
}

func (r *Repository) Remove(id uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.items, id)
	return r.save()
}

func (r *Repository) Items() []*media.Media {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return maps.Values(r.items)
}

func (r *Repository) Close() error {
	return nil
}

func (r *Repository) save() (err error) {
	if r.lockPath == "" {
		return nil
	}

	f, err := os.OpenFile(r.lockPath, os.O_WRONLY|os.O_CREATE, 0)
	if err != nil {
		return errors.Wrap(err, "failed to open index file")
	}
	defer func() {
		if err0 := f.Close(); err0 != nil {
			err = multierr.Append(err, errors.Wrap(err0, "failed to close index file"))
		}
	}()

	for _, m := range r.items {
		if err = r.write(f, m); err != nil {
			return err
		}
	}

	return err
}

func (r *Repository) saveSingle(m *media.Media) (err error) {
	if r.lockPath == "" {
		return nil
	}

	f, err := os.OpenFile(r.lockPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0)
	if err != nil {
		return errors.Wrap(err, "failed to open index file")
	}
	defer func() {
		if err0 := f.Close(); err0 != nil {
			err = multierr.Append(err, errors.Wrap(err0, "failed to close index file"))
		}
	}()

	err = r.write(f, m)
	return err
}

func (r *Repository) write(f *os.File, m *media.Media) error {
	path, err0 := filepath.Rel(r.path, m.Path)
	if err0 != nil {
		path = m.Path
	}

	b, err := json.Marshal(&media.Media{
		ID:     m.ID,
		Format: m.Format,
		Path:   path,
		Meta:   m.Meta,
	})
	if err != nil {
		return errors.Wrap(err, "failed to serialize index item")
	}

	if _, err = f.Write(append(b, []byte("\n")...)); err != nil {
		return errors.Wrap(err, "failed to write index item")
	}

	return nil
}
