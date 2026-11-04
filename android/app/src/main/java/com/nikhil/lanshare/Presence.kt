package com.nikhil.lanshare

import android.content.Context
import android.net.ConnectivityManager
import android.net.nsd.NsdManager
import android.net.nsd.NsdServiceInfo
import android.os.Handler
import android.os.Looper
import java.net.Inet4Address
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.TimeUnit

/**
 * Keeps track of which paired laptops are online while the app is open.
 *
 * Where to look, best first: what mDNS discovery just heard, where it answered last time,
 * and — if a laptop has gone quiet — a quick scan of this Wi-Fi network. A laptop is
 * "online" only after it answers a ping with the key we paired with, so a wrong or
 * spoofed address can never show it as online.
 */
class Presence(private val ctx: Context, private val myName: String, private val onChange: () -> Unit) {

    /**
     * ONE_WAY: the laptop reaches this phone (it connected to us recently) but we can't
     * reach it. That's nearly always Windows Firewall on the laptop blocking LAN Share.
     */
    enum class State { CHECKING, ONLINE, ONE_WAY, OFFLINE }

    data class Status(val state: State, val host: String? = null, val lastSeen: Long = 0)

    private val main = Handler(Looper.getMainLooper())
    private val statuses = ConcurrentHashMap<String, Status>()
    private val misses = ConcurrentHashMap<String, Int>()
    private val discovered = ConcurrentHashMap<String, Pair<String, Int>>() // id → (host, port)

    /** A LAN Share device announcing itself on this Wi-Fi (paired or not). */
    data class Nearby(val id: String, val name: String, val host: String, val port: Int, val platform: String)
    private val nearby = ConcurrentHashMap<String, Nearby>()

    /** Devices seen on this Wi-Fi that aren't paired yet (for "Pair a phone nearby"). */
    fun nearbyUnpaired(): List<Nearby> = nearby.values.filter { DeviceStore.get(ctx, it.id) == null }.sortedBy { it.name }
    @Volatile private var lastScan = 0L
    private var exec: ScheduledExecutorService? = null

    private val nsd = ctx.getSystemService(NsdManager::class.java)
    private var discovery: NsdManager.DiscoveryListener? = null
    private val resolveQueue = ArrayDeque<NsdServiceInfo>()
    private var resolving = false

    fun status(id: String): Status = statuses[id] ?: Status(State.CHECKING)

    // When a laptop connects to us, re-check it at once (it may have been offline).
    private val onHub: () -> Unit = {
        if (DeviceStore.all(ctx).any { status(it.id).state != State.ONLINE && System.currentTimeMillis() - Hub.lastInbound(it.id) < 3_000 }) checkNow()
    }

    fun start() {
        if (exec != null) return
        Hub.listen(onHub)
        exec = Executors.newSingleThreadScheduledExecutor().also {
            it.scheduleWithFixedDelay({ runCatching { checkAll() } }, 0, CHECK_EVERY_S, TimeUnit.SECONDS)
        }
        startDiscovery()
    }

    fun stop() {
        Hub.unlisten(onHub)
        exec?.shutdownNow()
        exec = null
        stopDiscovery()
    }

    /** Re-check everyone now (e.g. right after pairing). */
    fun checkNow() {
        exec?.execute { runCatching { checkAll() } }
    }

    private fun checkAll() {
        val devices = DeviceStore.all(ctx)
        var anyOffline = false
        for (d in devices) if (!check(d)) anyOffline = true
        if (anyOffline && System.currentTimeMillis() - lastScan > SCAN_COOLDOWN_MS) {
            lastScan = System.currentTimeMillis()
            if (scan()) for (d in DeviceStore.all(ctx)) check(d)
        }
    }

    /** Pings a device at each address worth trying; returns true if it answered. */
    private fun check(d: Device): Boolean {
        val candidates = linkedSetOf<Pair<String, Int>>()
        discovered[d.id]?.let { candidates += it }
        statuses[d.id]?.host?.let { candidates += it to d.port }
        if (d.host.isNotEmpty()) candidates += d.host to d.port
        Hub.inboundHost(d.id)?.let { candidates += it to d.port } // last: just a hint
        for ((host, port) in candidates) {
            val name = Client.pingName(d, host, port, myName)
            if (name != null) {
                if (name.isNotBlank() && name != d.name) { // renamed on that device
                    DeviceStore.get(ctx, d.id)?.let { DeviceStore.put(ctx, it.copy(name = name)) }
                    android.util.Log.i("LanShare", "presence: ${d.name} is now called $name")
                    main.post(onChange)
                }
                Hub.reached(d.id, true)
                DeviceStore.updateAddress(ctx, d.id, host, port)
                misses[d.id] = 0
                set(d.id, Status(State.ONLINE, host, System.currentTimeMillis()))
                return true
            }
        }
        Hub.reached(d.id, false)
        if (Hub.canReach(d.id) == false && System.currentTimeMillis() - Hub.lastInbound(d.id) < 20_000) {
            set(d.id, Status(State.ONE_WAY, null, System.currentTimeMillis()))
            return true // it's there; no point scanning the network for it
        }
        val m = (misses[d.id] ?: 0) + 1
        misses[d.id] = m
        // One missed ping can be Wi-Fi noise; call it offline after two.
        if (m >= 2) set(d.id, Status(State.OFFLINE, null, statuses[d.id]?.lastSeen ?: 0))
        return false
    }

    private fun set(id: String, s: Status) {
        val old = statuses.put(id, s)
        if (old?.state != s.state) android.util.Log.i("LanShare", "presence: $id ${old?.state} -> ${s.state} ${s.host ?: ""}")
        if (old?.state != s.state || old.host != s.host) main.post(onChange)
    }

    /** Looks for LAN Share devices on this Wi-Fi's /24. Returns true if a paired one turned up. */
    private fun scan(): Boolean {
        val myIp = wifiAddress() ?: return false
        val prefix = myIp.substringBeforeLast('.')
        val paired = DeviceStore.all(ctx).map { it.id }.toSet()
        val pool = Executors.newFixedThreadPool(48)
        var found = false
        val tasks = (1..254).map { n ->
            val host = "$prefix.$n"
            pool.submit {
                val id = PeerConnection.probe(host, Protocol.DEFAULT_PORT)
                if (id != null && id in paired) {
                    discovered[id] = host to Protocol.DEFAULT_PORT
                    found = true
                }
            }
        }
        tasks.forEach { runCatching { it.get(5, TimeUnit.SECONDS) } }
        pool.shutdownNow()
        return found
    }

    private fun wifiAddress(): String? {
        val cm = ctx.getSystemService(ConnectivityManager::class.java)
        val props = cm.getLinkProperties(cm.activeNetwork) ?: return null
        return props.linkAddresses.map { it.address }
            .firstOrNull { it is Inet4Address && !it.isLoopbackAddress && it.isSiteLocalAddress }
            ?.hostAddress
    }

    // ---------- mDNS discovery (Android's NsdManager) ----------

    private fun startDiscovery() {
        val listener = object : NsdManager.DiscoveryListener {
            override fun onDiscoveryStarted(serviceType: String) {}
            override fun onDiscoveryStopped(serviceType: String) {}
            override fun onStartDiscoveryFailed(serviceType: String, errorCode: Int) {}
            override fun onStopDiscoveryFailed(serviceType: String, errorCode: Int) {}
            override fun onServiceLost(info: NsdServiceInfo) {} // pings decide what's offline
            override fun onServiceFound(info: NsdServiceInfo) = enqueueResolve(info)
        }
        discovery = listener
        runCatching { nsd.discoverServices(Protocol.SERVICE_TYPE, NsdManager.PROTOCOL_DNS_SD, listener) }
    }

    private fun stopDiscovery() {
        discovery?.let { runCatching { nsd.stopServiceDiscovery(it) } }
        discovery = null
        synchronized(resolveQueue) { resolveQueue.clear() }
    }

    // Older Android versions can only resolve one service at a time, so queue them.
    private fun enqueueResolve(info: NsdServiceInfo) {
        synchronized(resolveQueue) {
            resolveQueue.addLast(info)
            if (!resolving) resolveNext()
        }
    }

    @Suppress("DEPRECATION") // resolveService works on every version we support
    private fun resolveNext() {
        val next = synchronized(resolveQueue) {
            val n = resolveQueue.removeFirstOrNull()
            resolving = n != null
            n
        } ?: return
        runCatching {
            nsd.resolveService(next, object : NsdManager.ResolveListener {
                override fun onResolveFailed(info: NsdServiceInfo, errorCode: Int) = resolveNext()
                override fun onServiceResolved(info: NsdServiceInfo) {
                    val id = info.attributes["id"]?.toString(Charsets.UTF_8)
                    val host = info.host?.hostAddress
                    if (id != null && host != null && info.host is Inet4Address && id != runCatching { Identity.fingerprint }.getOrNull()) {
                        nearby[id] = Nearby(id, info.serviceName, host, info.port,
                            info.attributes["p"]?.toString(Charsets.UTF_8) ?: "windows")
                        val old = discovered.put(id, host to info.port)
                        if (old != host to info.port && DeviceStore.get(ctx, id) != null) checkNow()
                    }
                    resolveNext()
                }
            })
        }.onFailure { resolveNext() }
    }

    companion object {
        private const val CHECK_EVERY_S = 5L
        private const val SCAN_COOLDOWN_MS = 30_000L
    }
}
