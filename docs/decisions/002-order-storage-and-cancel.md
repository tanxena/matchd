# 002 — Order storage and cancel lookup: indexed slab with uint32 links

- **Status:** accepted (milestone 1)
- **Code:** `internal/book/book.go` (`orderNode`, `Book.nodes`, `Book.alloc`, `Book.release`, `Book.unlink`, `Book.Cancel`)

## Context

Within a price level, orders form a FIFO queue. The queue needs three operations:

- remove the head when it is fully filled;
- append at the tail when an order rests;
- remove from the middle when an order is cancelled, which needs O(1) access to an arbitrary order by `order_id`.

PRD 6 also requires zero allocations on the steady-state add, cancel and match paths. Milestone 5 will need to copy the book cheaply for snapshots.

## Options considered

1. **`map[OrderID]*node` and a pointer-linked doubly linked list per level.** This is the textbook design: O(1) unlink through the node's prev/next pointers.
   - Each new resting order allocates a node unless a separate pool is added.
   - Every node holds pointers, so the garbage collector must trace all resting orders on every cycle; GC cost grows with book depth.
   - Copying the book for a snapshot means rebuilding a pointer graph.

   Rejected.
2. **Indexed slab with `uint32` prev/next indices, a free list, and `map[OrderID]uint32`** (chosen).
   - All orders live by value in a single `[]orderNode`.
   - Queues are linked by index, not pointer. Index `0` is reserved as nil, so the zero value of a link means "none".
   - Freed slots are chained through their own `next` field into a free list and reused before the slab grows.
3. **A slab indexed directly by `order_id`**, with no map. `order_id` grows forever while slots are recycled, so this would need `order_id mod capacity` plus generation checks. That is more machinery than one map lookup. Rejected.

## Decision

Option 2.

- `orderNode` holds id, client order id, client, side, price, remaining qty, and `prev`/`next uint32`.
- `Book.byID map[OrderID]uint32` finds the slot for a cancel.
- `Book.byClOrd map[clientOrderKey]uint32` enforces the duplicate-client-order-id rule (PRD 5.1).
- `release` zeroes the slot before pushing it on the free list, so a stale index can never read a plausible-looking order.

**Cancel cost:**
- O(1): one map lookup and an O(1) unlink;
- O(log L): a binary search to find the order's level, because a node doesn't store a pointer to its level;
- if the level empties, the O(L − i) slice removal from decision 001.

So cancel is **O(1) for the order plus O(log L) for the level** in the common case. That answers PRD 5.2's "O(1) or O(log n); document which".

## Costs

- **Map growth can allocate.** Go maps don't shrink, and once a map has grown to the book's peak size, delete-then-insert cycles reuse its storage. Steady state is allocation-free; growing to a new peak is not. Milestone 2 proves this with `testing.AllocsPerRun`.
- **Slab growth can allocate.** `append` doubles the backing array when the book reaches a new peak order count. The pointer `m := &b.nodes[i]` is only valid until the next growth, so `Submit` finishes matching, which never grows the slab, before calling `rest`, which may. This rule is commented in the code and is one of the "what breaks if" scenarios in `docs/learn/01-book-core.md`.
- **2³²−1 resting orders at most per book.** `alloc` panics beyond that. A single instrument resting four billion orders is outside any realistic scope.
- **No level pointer in the node.** This keeps `orderNode` pointer-free, so the GC never scans the slab. The price is the O(log L) level search on cancel.
- **Two map lookups per accepted order:** the duplicate check on arrival, and an insert when it rests. This is the price of the PRD 5.1 rule.
