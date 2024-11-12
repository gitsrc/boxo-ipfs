package providerquerymanager

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/libp2p/go-libp2p/core/peer"
)

type counter struct {
	counts map[peer.ID]int64
	mutex  sync.Mutex
}

func newCounter() *counter {
	return &counter{
		counts: make(map[peer.ID]int64),
	}
}

func (c *counter) add(k peer.ID, count int64) int64 {
	c.mutex.Lock()
	count += c.counts[k]
	c.counts[k] = count
	c.mutex.Unlock()
	return count
}

// topN sorts counters in descending order and returns a string with the
// highest N counters.
func (c *counter) topN(n int, sep string) string {
	type item struct {
		key   peer.ID
		value int64
	}
	var i int

	c.mutex.Lock()
	if len(c.counts) == 0 {
		c.mutex.Unlock()
		return ""
	}
	items := make([]item, len(c.counts))
	for k, v := range c.counts {
		items[i] = item{
			key:   k,
			value: v,
		}
		i++
	}
	c.mutex.Unlock()

	slices.SortFunc(items, func(a, b item) int {
		return cmp.Compare(b.value, a.value)
	})

	counts := make([]string, 0, n)
	for i := range items {
		if i == n {
			break
		}
		counts = append(counts, fmt.Sprint(items[i].key, ": ", items[i].value))
	}
	return strings.Join(counts, sep)
}
