package replacer

// sentinelTrie 占位符/仿真值前缀树。
//
// 用途：SSE 流被任意切分时，占位符可能被切成两半（如 "<<zh_person_na" + "me_1>>"），
// trie 负责跨块拼接完整哨兵串后再查映射表还原（契约 §5.3、技术方案 §6.2 规则 4）。
type sentinelTrie struct {
	root *trieNode
}

type trieNode struct {
	children map[byte]*trieNode
	value    string // 哨兵串对应的还原目标（原值）；空表示非终结
	terminal bool
}

func newSentinelTrie() *sentinelTrie {
	return &sentinelTrie{root: &trieNode{children: make(map[byte]*trieNode)}}
}

// Insert 插入一条「上游可见串 → 还原目标」映射。
func (t *sentinelTrie) Insert(sentinel, restoreTo string) {
	n := t.root
	for i := 0; i < len(sentinel); i++ {
		c := sentinel[i]
		next, ok := n.children[c]
		if !ok {
			next = &trieNode{children: make(map[byte]*trieNode)}
			n.children[c] = next
		}
		n = next
	}
	n.terminal = true
	n.value = restoreTo
}

// hasStart 是否存在以该字节开头的哨兵串。
func (t *sentinelTrie) hasStart(c byte) bool {
	_, ok := t.root.children[c]
	return ok
}

// longestMatch 返回 buf 前缀能匹配到的最长哨兵串长度与其还原目标。
func (t *sentinelTrie) longestMatch(buf []byte) (int, string, bool) {
	n := t.root
	bestLen, bestVal, found := 0, "", false
	for i := 0; i < len(buf); i++ {
		next, ok := n.children[buf[i]]
		if !ok {
			break
		}
		n = next
		if n.terminal {
			bestLen, bestVal, found = i+1, n.value, true
		}
	}
	return bestLen, bestVal, found
}

// isPrefix buf 整体是否为某个哨兵串的前缀（需要更多数据才能判定）。
func (t *sentinelTrie) isPrefix(buf []byte) bool {
	n := t.root
	for i := 0; i < len(buf); i++ {
		next, ok := n.children[buf[i]]
		if !ok {
			return false
		}
		n = next
	}
	return true
}
