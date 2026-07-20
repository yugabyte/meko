type QueueNode<T> = {
  value: T;
  next: QueueNode<T> | undefined;
};

/**
 * Minimal generic FIFO queue, à la https://github.com/sindresorhus/yocto-queue.
 *
 * A plain singly-linked list of single-value nodes -- no backing array, so
 * no `Array.shift()` O(N) re-indexing and no resize/grow-shrink bookkeeping
 * to maintain. That trade-off (one small node object per item) only pays
 * off for a queue this small; this one's caller never holds more than a
 * handful of items at once, so simplicity wins over the ring-buffer/block
 * approaches that make sense for high-throughput or large queues.
 */
export class Queue<T> {
  private head: QueueNode<T> | undefined;
  private tail: QueueNode<T> | undefined;
  private count = 0;

  /** Adds an item to the back of the queue. */
  enqueue(item: T): void {
    const node: QueueNode<T> = { value: item, next: undefined };
    if (this.tail) {
      this.tail.next = node;
    } else {
      this.head = node;
    }
    this.tail = node;
    this.count += 1;
  }

  /** Removes and returns the item at the front of the queue, or `undefined` if empty. */
  dequeue(): T | undefined {
    const node = this.head;
    if (!node) return undefined;
    this.head = node.next;
    if (!this.head) this.tail = undefined;
    this.count -= 1;
    return node.value;
  }

  /** Returns the item at the front of the queue without removing it, or `undefined` if empty. */
  peek(): T | undefined {
    return this.head?.value;
  }

  /** True when there are no items left to dequeue. */
  isEmpty(): boolean {
    return this.count === 0;
  }

  /** Number of items currently queued. */
  size(): number {
    return this.count;
  }

  /** Removes every queued item. */
  clear(): void {
    this.head = undefined;
    this.tail = undefined;
    this.count = 0;
  }

  /** Snapshot of the remaining items, front to back. Does not mutate the queue. */
  toArray(): T[] {
    const out: T[] = [];
    let node = this.head;
    while (node) {
      out.push(node.value);
      node = node.next;
    }
    return out;
  }
}
