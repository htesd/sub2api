package service

import (
	"crypto/sha256"
	"fmt"

	"github.com/gin-gonic/gin"
)

// Keep only hashes of opaque routing tokens. A bounded FIFO preserves ownership
// across retries and later requests that still echo an earlier account's token.
// Cache misses are hints, not authentication: strict response IDs are separate.
func (r *codexCapacityRegistry) foreignTurnState(key int64, token, identity, inferredOwner string) bool {
	return r.turnStateOwnership(key, token, identity, inferredOwner, false)
}

func noteCodexCapacityTurnState(c *gin.Context, account *Account, token string) {
	if lease := capacityLeaseForAccount(c, account); lease != nil {
		lease.registry.turnStateOwnership(lease.key.Key, token, lease.identity, lease.identity, true)
	}
}

func (r *codexCapacityRegistry) turnStateOwnership(key int64, token, identity, inferredOwner string, emitted bool) bool {
	if token == "" {
		return false
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", key, token)))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.turnOwners == nil {
		r.turnOwners = make(map[[32]byte]string)
	}
	owner, ok := r.turnOwners[digest]
	if ok && emitted {
		owner = identity
		r.turnOwners[digest] = owner
	}
	if !ok {
		owner = inferredOwner
		if len(r.turnOrder) < codexCapacityMaxBindings {
			r.turnOrder = append(r.turnOrder, digest)
		} else {
			delete(r.turnOwners, r.turnOrder[r.turnNext])
			r.turnOrder[r.turnNext] = digest
			r.turnNext = (r.turnNext + 1) % len(r.turnOrder)
		}
		r.turnOwners[digest] = owner
	}
	return owner != identity
}
