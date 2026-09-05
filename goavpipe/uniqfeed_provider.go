package goavpipe

import (
	"sync"

	"github.com/modern-go/gls"
)

type UniqfeedMetadataProvider interface {
	Init(projectPath, metadataDir string) error
	GetMetadataBlob(frameIndex uint64, streamIndex uint32, renderTID int64) ([]byte, error)
	Close() error
}

type QueuedUniqfeedMetadataProvider struct {
	mu          sync.Mutex
	byRenderTID map[int64][]byte
	closed      bool
}

func NewQueuedUniqfeedMetadataProvider() *QueuedUniqfeedMetadataProvider {
	return &QueuedUniqfeedMetadataProvider{
		byRenderTID: make(map[int64][]byte),
	}
}

func (p *QueuedUniqfeedMetadataProvider) Init(projectPath, metadataDir string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = false
	return nil
}

func (p *QueuedUniqfeedMetadataProvider) GetMetadataBlob(frameIndex uint64, streamIndex uint32, renderTID int64) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	blob, ok := p.byRenderTID[renderTID]
	if !ok {
		return nil, nil
	}

	delete(p.byRenderTID, renderTID)
	return append([]byte(nil), blob...), nil
}

func (p *QueuedUniqfeedMetadataProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	clear(p.byRenderTID)
	return nil
}

func (p *QueuedUniqfeedMetadataProvider) Push(renderTID int64, blob []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.byRenderTID[renderTID] = append([]byte(nil), blob...)
}

var handleUniqfeedProviders sync.Map
var pendingUniqfeedProviders sync.Map

func RegisterPendingUniqfeedMetadataProvider(provider UniqfeedMetadataProvider) {
	if provider == nil {
		return
	}
	pendingUniqfeedProviders.Store(gls.GoID(), provider)
}

func AttachPendingUniqfeedMetadataProvider(handle int32) {
	if handle <= 0 {
		return
	}
	provider, ok := pendingUniqfeedProviders.LoadAndDelete(gls.GoID())
	if !ok {
		return
	}
	handleUniqfeedProviders.Store(handle, provider)
}

func ClearPendingUniqfeedMetadataProvider() {
	pendingUniqfeedProviders.Delete(gls.GoID())
}

func RegisterUniqfeedMetadataProvider(handle int32, provider UniqfeedMetadataProvider) {
	if handle <= 0 || provider == nil {
		return
	}
	handleUniqfeedProviders.Store(handle, provider)
}

func GetUniqfeedMetadataProvider(handle int32) (UniqfeedMetadataProvider, bool) {
	provider, ok := handleUniqfeedProviders.Load(handle)
	if !ok {
		return nil, false
	}
	return provider.(UniqfeedMetadataProvider), true
}

func ReleaseUniqfeedMetadataProvider(handle int32) error {
	provider, ok := handleUniqfeedProviders.LoadAndDelete(handle)
	if !ok {
		return nil
	}
	return provider.(UniqfeedMetadataProvider).Close()
}
