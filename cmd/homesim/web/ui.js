// Pure view logic shared with Node tests. No DOM, credentials, or device access.
(function (root) {
  function conversations(rows, peers, filter, query) {
    const grouped = new Map();
    for (const m of rows) {
      if (!grouped.has(m.peer)) grouped.set(m.peer, {peer: m.peer, latest: m, count: 0, unread: 0, ...peers[m.peer]});
      const c = grouped.get(m.peer);
      c.count++;
      if (m.direction === 'incoming' && !m.read) c.unread++;
      if (new Date(m.timestamp) > new Date(c.latest.timestamp)) c.latest = m;
    }
    const q = query.trim().toLocaleLowerCase();
    return [...grouped.values()].filter(c => {
      if (filter === 'archived' ? !c.archived : c.archived) return false;
      if (filter === 'unread' && !c.unread || filter === 'starred' && !c.starred) return false;
      return !q || (c.name || '').toLocaleLowerCase().includes(q) || c.peer.toLocaleLowerCase().includes(q) || rows.some(m => m.peer === c.peer && m.content.toLocaleLowerCase().includes(q));
    }).sort((a, b) => new Date(b.latest.timestamp) - new Date(a.latest.timestamp));
  }
  function unreadIDs(rows, peer) { return rows.filter(m => m.direction === 'incoming' && !m.read && (!peer || m.peer === peer)).map(m => m.id); }
  function statusLabel(s) { return {submitted: '已提交运营商', pending: '提交中', unknown: '结果未知，请先核对，勿重复发送', received: '已收到'}[s] || s; }
  function dayLabel(timestamp, now = new Date()) {
    const date = new Date(timestamp), today = new Date(now);
    if (date.toDateString() === today.toDateString()) return '今天';
    today.setDate(today.getDate() - 1);
    if (date.toDateString() === today.toDateString()) return '昨天';
    return date.toLocaleDateString('zh-CN', {year: 'numeric', month: 'long', day: 'numeric'});
  }
  const api = {conversations, unreadIDs, statusLabel, dayLabel};
  root.HomeSIMUI = api;
  if (typeof module !== 'undefined') module.exports = api;
})(globalThis);
