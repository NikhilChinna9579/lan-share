package com.nikhil.lanshare

import android.os.Handler
import android.os.Looper
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.CopyOnWriteArrayList

/**
 * What's happening right now, shared by the receiving service and the screen (same
 * process): transfers in both directions, a short activity list, and who reached whom.
 * The screen listens and redraws as things change, so status is always live.
 */
object Hub {
    enum class State { RUNNING, DONE, ERROR, CANCELLED }

    class Transfer(
        val id: String, val inbound: Boolean, val device: String,
        @Volatile var name: String = "", @Volatile var sent: Long = 0, @Volatile var total: Long = 0,
        @Volatile var files: Int = 0, @Volatile var state: State = State.RUNNING,
        @Volatile var message: String = "", @Volatile var folder: String = "",
        @Volatile var index: Int = 1, @Volatile var count: Int = 1,
        val started: Long = System.currentTimeMillis(), @Volatile var updated: Long = System.currentTimeMillis(),
        @Volatile var elapsed: Long = 0,            // ms, set when it ends
        @Volatile var cancel: (() -> Unit)? = null, // stops it while running
        @Volatile var retry: (() -> Unit)? = null,  // sends it again (outgoing only)
        @Volatile var cancelled: Boolean = false,
    ) {
        val speed get() = if (state == State.DONE && elapsed > 0) total * 1000 / elapsed
            else sent * 1000 / maxOf(System.currentTimeMillis() - started, 1)
    }

    data class Item(val time: Long, val kind: String, val text: String, val detail: String = "")

    private val main = Handler(Looper.getMainLooper())
    private val listeners = CopyOnWriteArrayList<() -> Unit>()
    private val transfers = LinkedHashMap<String, Transfer>()
    private val items = ArrayDeque<Item>()
    @Volatile private var posted = false

    // Reachability, both ways (for "can't connect" diagnosis).
    private val inbound = ConcurrentHashMap<String, Long>()   // laptop id → when it last connected to us
    private val reachOk = ConcurrentHashMap<String, Long>()   // when we last reached it
    private val reachFail = ConcurrentHashMap<String, Long>() // when we last failed to

    fun listen(l: () -> Unit) = listeners.add(l)
    fun unlisten(l: () -> Unit) = listeners.remove(l)

    /** Redraw soon (coalesced: at most ~7 times a second). */
    fun changed() {
        if (posted) return
        posted = true
        main.postDelayed({ posted = false; listeners.forEach { it() } }, 150)
    }

    @Synchronized
    fun transfer(id: String, inbound: Boolean, device: String, update: (Transfer) -> Unit) {
        val isNew = id !in transfers
        val t = transfers.getOrPut(id) { Transfer(id, inbound, device) }
        val before = t.state
        update(t)
        // One line per start and per end: what tests (and bug reports) go by.
        if (isNew) android.util.Log.i("LanShare", "transfer ${if (inbound) "in" else "out"} started: ${t.name} (${t.count} files) ${if (inbound) "from" else "to"} $device")
        if (t.state != before) android.util.Log.i("LanShare", "transfer ${if (inbound) "in" else "out"} ${t.state}: ${t.name} ${t.message}")
        t.updated = System.currentTimeMillis()
        if (t.state != State.RUNNING && t.elapsed == 0L) t.elapsed = t.updated - t.started
        while (transfers.size > 6) transfers.remove(transfers.keys.first())
        changed()
    }

    /** Running transfers, finished ones for a few seconds, failed ones until dismissed. Newest first. */
    @Synchronized
    fun visibleTransfers(): List<Transfer> {
        val now = System.currentTimeMillis()
        return transfers.values.filter {
            it.state == State.RUNNING || now - it.updated < 8_000 ||
                (it.state != State.DONE && it.retry != null && now - it.updated < 30 * 60_000)
        }.reversed()
    }

    @Synchronized
    fun dismiss(id: String) { transfers.remove(id); changed() }

    @Synchronized
    fun get(id: String): Transfer? = transfers[id]

    @Synchronized
    fun add(kind: String, text: String, detail: String = "") {
        items.addFirst(Item(System.currentTimeMillis(), kind, text, detail))
        android.util.Log.i("LanShare", "activity: $kind: $text · $detail")
        while (items.size > 30) items.removeLast()
        save()
        changed()
    }

    // History survives app restarts: kept in the app's private preferences.
    private var prefs: android.content.SharedPreferences? = null

    @Synchronized
    fun init(ctx: android.content.Context) {
        if (prefs != null) return
        prefs = ctx.applicationContext.getSharedPreferences("history", android.content.Context.MODE_PRIVATE)
        val arr = org.json.JSONArray(prefs!!.getString("items", "[]"))
        if (items.isEmpty()) for (i in 0 until arr.length()) {
            val o = arr.getJSONObject(i)
            items.addLast(Item(o.getLong("t"), o.getString("k"), o.getString("x"), o.optString("d")))
        }
    }

    private fun save() {
        val arr = org.json.JSONArray()
        items.forEach { arr.put(org.json.JSONObject().put("t", it.time).put("k", it.kind).put("x", it.text).put("d", it.detail)) }
        prefs?.edit()?.putString("items", arr.toString())?.apply()
    }

    @Synchronized
    fun recent(): List<Item> = items.toList()

    fun inboundContact(id: String) {
        val was = inbound.put(id, System.currentTimeMillis())
        if (was == null || System.currentTimeMillis() - was > 15_000) changed()
    }

    fun lastInbound(id: String): Long = inbound[id] ?: 0

    private val inboundHost = ConcurrentHashMap<String, String>()
    fun inboundFrom(id: String, host: String) { inboundHost[id] = host }
    fun inboundHost(id: String): String? = inboundHost[id]

    // The file(s) last shared into LAN Share: what "this" means to the assistant for a while.
    @Volatile private var lastShared: Pair<Long, List<android.net.Uri>>? = null
    fun shared(uris: List<android.net.Uri>) { lastShared = System.currentTimeMillis() to uris }
    fun recentlyShared(): List<android.net.Uri>? =
        lastShared?.takeIf { System.currentTimeMillis() - it.first < 10 * 60_000 }?.second

    private val failStreak = ConcurrentHashMap<String, Int>()

    fun reached(id: String, ok: Boolean) {
        (if (ok) reachOk else reachFail)[id] = System.currentTimeMillis()
        failStreak[id] = if (ok) 0 else (failStreak[id] ?: 0) + 1
    }

    /** Can this phone connect to that laptop? null if we don't know (yet). */
    fun canReach(id: String): Boolean? {
        val ok = reachOk[id] ?: 0
        val fail = reachFail[id] ?: 0
        return when {
            fail > ok && (failStreak[id] ?: 0) >= 2 && System.currentTimeMillis() - fail < 120_000 -> false
            ok > 0 -> true
            else -> null
        }
    }
}
