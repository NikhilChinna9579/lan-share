package com.nikhil.lanshare

import android.Manifest
import android.animation.ObjectAnimator
import android.animation.ValueAnimator
import android.app.Activity
import android.content.Intent
import android.content.pm.ApplicationInfo
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.speech.RecognitionListener
import android.speech.RecognizerIntent
import android.speech.SpeechRecognizer
import android.text.SpannableString
import android.text.Spanned
import android.text.style.ForegroundColorSpan
import android.util.Log
import android.view.KeyEvent
import android.view.LayoutInflater
import android.view.View
import android.view.inputmethod.EditorInfo
import android.view.inputmethod.InputMethodManager
import android.widget.EditText
import android.widget.ImageButton
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.ProgressBar
import android.widget.TextView
import java.io.IOException
import java.net.ConnectException
import java.net.NoRouteToHostException
import java.net.SocketTimeoutException
import javax.net.ssl.SSLException

/**
 * The assistant, opened from the floating bubble. Speak (or type) things like
 * "send this to my laptop", "send the last photo to my laptop in Documents" or
 * "get report.pdf from my laptop". Anything missing is asked as a follow-up
 * (which laptop? which folder? which file?), answered by voice or by tapping.
 * Before anything is sent there's a 2-second pause to cancel.
 *
 * Speech: Android's recognizer, offline first; if the offline model isn't there,
 * it falls back to the online one (hybrid). Understanding: [Commands], on the phone.
 */
class AssistantActivity : Activity() {
    private val main = Handler(Looper.getMainLooper())
    private val worker get() = Actions.worker
    private val myName by lazy { deviceName(this) }
    private lateinit var presence: Presence

    private lateinit var prompt: TextView
    private lateinit var detail: TextView
    private lateinit var bar: ProgressBar
    private lateinit var choices: LinearLayout
    private lateinit var input: EditText
    private lateinit var mic: ImageButton
    private lateinit var devicesRow: LinearLayout

    /** A choice offered on screen; while [asking], saying its label picks it too. */
    private class Option(val label: String, val icon: Int = R.drawable.ic_chevron, val sub: String? = null,
                         val chip: String? = null, val run: (() -> Unit)?)

    private var options: List<Option> = emptyList()
    private var asking = false
    private var countdown: Runnable? = null
    private var countdownAnim: ValueAnimator? = null
    private var watchFrom = 0L          // show progress of transfers started after this
    private var closeWhenDone = false
    private var statusMode = false
    private var picking: ((List<Client.Outgoing>) -> Unit)? = null

    private var recognizer: SpeechRecognizer? = null
    private var listening = false
    private var preferOffline = true
    private var pulse: ObjectAnimator? = null
    private val onHub: () -> Unit = { showProgress() }

    private fun ui(block: () -> Unit) {
        main.post { if (!isFinishing && !isDestroyed) block() }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_assistant)
        prompt = findViewById(R.id.prompt)
        detail = findViewById(R.id.detail)
        bar = findViewById(R.id.bar)
        choices = findViewById(R.id.choices)
        input = findViewById(R.id.input)
        mic = findViewById(R.id.mic)
        devicesRow = findViewById(R.id.devices)
        val panel = findViewById<View>(R.id.panel)
        panel.setOnApplyWindowInsetsListener { v, insets ->
            v.setPadding(v.paddingLeft, v.paddingTop, v.paddingRight, systemBottomInset(insets) + dp(16))
            insets
        }
        findViewById<View>(R.id.root).setOnClickListener { finish() }
        findViewById<View>(R.id.close).setOnClickListener { finish() }
        mic.setOnClickListener { if (listening) stopListening() else listen() }
        input.setOnEditorActionListener { _, action, ev ->
            if (action == EditorInfo.IME_ACTION_GO || ev?.keyCode == KeyEvent.KEYCODE_ENTER) {
                val t = input.text.toString().trim()
                if (t.isNotEmpty()) handle(t)
                true
            } else false
        }

        Hub.init(this)
        presence = Presence(this, myName) { ui { renderDevices(); if (statusMode) status(null) } }
        idle()
        renderDevices()
        // Debug builds: tests pass a sentence instead of speaking it.
        val say = intent.getStringExtra("say")
        if (say != null && applicationInfo.flags and ApplicationInfo.FLAG_DEBUGGABLE != 0) main.postDelayed({ handle(say) }, 600)
    }

    override fun onStart() {
        super.onStart()
        Bubble.hiddenForAssistant = true
        Bubble.update()
        Hub.listen(onHub)
        presence.start()
    }

    override fun onStop() {
        Bubble.hiddenForAssistant = false
        Bubble.update()
        Hub.unlisten(onHub)
        presence.stop()
        stopListening()
        super.onStop()
        if (picking == null && !isChangingConfigurations) finish() // a quick panel, not a place to come back to
    }

    override fun onDestroy() {
        cancelCountdown()
        recognizer?.destroy()
        super.onDestroy()
    }

    // ------------------------------------------------------------------ screen

    private fun idle() {
        asking = false
        statusMode = false
        bar.visibility = View.GONE
        val d = DeviceStore.all(this)
        if (d.isEmpty()) {
            say("Pair a laptop first", "Open LAN Share and scan the code your laptop shows.")
            return
        }
        val name = d.singleOrNull()?.name ?: "my laptop"
        prompt.text = "What should I do?"
        Log.i(TAG, "assistant: ready")
        detail.text = "Tap the mic and say, for example, “send this to $name”."
        val items = mutableListOf(
            Option("Send the last photo", R.drawable.ic_image, sub = "to $name") { handle("send the last photo to $name") },
            Option("Send the last screenshot", R.drawable.ic_image, sub = "to $name") { handle("send the last screenshot to $name") },
            Option("Send a file…", R.drawable.ic_send) { pickFiles { handleItems(it) } },
        )
        Hub.recent().take(2).forEach { items += Option(it.text, R.drawable.ic_check_circle, sub = it.detail.ifEmpty { null }, run = null) }
        show(items, asking = false)
    }

    private fun say(title: String, sub: String = "") {
        prompt.text = title
        detail.text = sub
        detail.visibility = if (sub.isEmpty()) View.GONE else View.VISIBLE
        Log.i(TAG, "assistant: $title${if (sub.isNotEmpty()) " — $sub" else ""}")
    }

    private fun show(list: List<Option>, asking: Boolean) {
        this.options = list
        this.asking = asking
        choices.removeAllViews()
        for (o in list) {
            val r = LayoutInflater.from(this).inflate(R.layout.item_sheet_row, choices, false)
            r.findViewById<ImageView>(R.id.rowIcon).setImageResource(o.icon)
            r.findViewById<TextView>(R.id.rowLabel).text = o.label
            r.findViewById<TextView>(R.id.rowSub).apply { text = o.sub; visibility = if (o.sub == null) View.GONE else View.VISIBLE }
            r.findViewById<TextView>(R.id.rowChip).apply { text = o.chip; visibility = if (o.chip == null) View.GONE else View.VISIBLE }
            r.findViewById<View>(R.id.rowChevron).visibility = if (o.run != null && asking) View.VISIBLE else View.GONE
            if (o.run != null) r.setOnClickListener { cancelCountdown(); o.run.invoke() } else r.isClickable = false
            choices.addView(r)
        }
        // Long lists scroll instead of pushing the mic off screen.
        choices.post {
            val max = (resources.displayMetrics.heightPixels * 0.4).toInt()
            val sv = findViewById<View>(R.id.choicesScroll)
            sv.layoutParams.height = if (choices.height > max) max else LinearLayout.LayoutParams.WRAP_CONTENT
            sv.requestLayout()
        }
    }

    private fun ask(question: String, sub: String, list: List<Option>) {
        say(question, sub)
        show(list + Option("Cancel", R.drawable.ic_close) { idle() }, asking = true)
        Log.i(TAG, "assistant: asking: $question (${list.joinToString { it.label }})")
    }

    private fun renderDevices() {
        devicesRow.removeAllViews()
        for (d in DeviceStore.all(this)) {
            val st = presence.status(d.id).state
            val color = getColor(when (st) {
                Presence.State.ONLINE -> R.color.online
                Presence.State.OFFLINE -> R.color.offline
                else -> R.color.checking
            })
            val text = SpannableString("●  ${d.name}")
            text.setSpan(ForegroundColorSpan(color), 0, 1, Spanned.SPAN_EXCLUSIVE_EXCLUSIVE)
            devicesRow.addView(TextView(this).apply {
                this.text = text
                textSize = 13f
                setTextColor(getColor(R.color.ink))
                setBackgroundResource(R.drawable.bg_chip)
                setPadding(dp(12), dp(6), dp(14), dp(6))
                contentDescription = "${d.name}: ${st.name.lowercase()}"
                layoutParams = LinearLayout.LayoutParams(LinearLayout.LayoutParams.WRAP_CONTENT, LinearLayout.LayoutParams.WRAP_CONTENT)
                    .apply { marginEnd = dp(8) }
            })
        }
    }

    // ------------------------------------------------------------------ understanding

    private fun handle(text: String) {
        Log.i(TAG, "assistant: heard \"$text\"")
        input.setText("")
        getSystemService(InputMethodManager::class.java).hideSoftInputFromWindow(input.windowToken, 0)
        cancelCountdown()
        closeWhenDone = false
        statusMode = false
        val t = Commands.normalize(text)
        if (Regex("^(cancel|stop|no|nope|never mind|nevermind|forget it)\\b").containsMatchIn(t)) { idle(); return }

        // An answer to a follow-up question ("Documents", "the office pc", "the second one").
        if (asking) {
            val runnable = options.filter { it.run != null && it.label != "Cancel" }
            val labels = runnable.map { it.label }
            val pick = Commands.findFolder(t, labels) ?: Commands.findDevice(t, labels)
                ?: ordinal(t)?.let { labels.getOrNull(it) }
                ?: labels.filter { Commands.nameScore(it, t) > 0 }.singleOrNull()
            if (pick != null) { runnable.first { it.label == pick }.run!!.invoke(); return }
        }

        val devices = DeviceStore.all(this)
        if (devices.isEmpty()) { say("Pair a laptop first", "Open LAN Share and scan the code your laptop shows."); return }
        when (val c = Commands.parse(text, devices.map { it.name })) {
            is Commands.Command.Send -> planSend(c)
            is Commands.Command.Get -> planGet(c)
            is Commands.Command.Status -> status(c.device)
            Commands.Command.Unknown -> {
                say("Sorry, I didn't get that", "Try “send the last photo to my laptop” or “get report.pdf from my laptop”.")
                if (asking) show(options, asking = true)
            }
        }
    }

    private fun ordinal(t: String): Int? = listOf("first|1st|one", "second|2nd|two", "third|3rd|three", "fourth|4th|four")
        .indexOfFirst { Regex("\\b($it)\\b").containsMatchIn(t) }.takeIf { it >= 0 }

    private fun status(name: String?) {
        statusMode = true
        val all = DeviceStore.all(this)
        val list = name?.let { n -> all.filter { it.name == n } } ?: all
        val lines = list.map { d ->
            when (presence.status(d.id).state) {
                Presence.State.ONLINE -> "${d.name} is online"
                Presence.State.ONE_WAY -> "${d.name} is on, but its firewall blocks this phone"
                Presence.State.OFFLINE -> "${d.name} is offline"
                Presence.State.CHECKING -> "Checking ${d.name}…"
            }
        }
        say(lines.firstOrNull() ?: "No laptops paired", lines.drop(1).joinToString("\n"))
        show(emptyList(), asking = false)
    }

    // ------------------------------------------------------------------ sending

    private fun planSend(c: Commands.Command.Send) {
        say("Finding the file…")
        show(emptyList(), asking = false)
        worker.execute {
            val uris: List<Uri>? = when (c.what) {
                Commands.What.THIS -> Hub.recentlyShared()
                Commands.What.NAMED -> Actions.findOnPhone(this, c.name!!)?.let { listOf(it) }
                else -> Actions.latest(this, c.what)?.let { listOf(it) }
            }
            val items = uris?.let { runCatching { Client.resolveFiles(this, it) }.getOrNull() }
            // Nothing shared: offer the newest photo and screenshot ("show me").
            val photo = if (items == null && c.what == Commands.What.THIS) Actions.latest(this, Commands.What.PHOTO) else null
            val shot = if (items == null && c.what == Commands.What.THIS) Actions.latest(this, Commands.What.SCREENSHOT) else null
            val photoItem = photo?.let { runCatching { Client.resolveFiles(this, listOf(it)) }.getOrNull() }
            val shotItem = shot?.let { runCatching { Client.resolveFiles(this, listOf(it)) }.getOrNull() }
            ui {
                if (!items.isNullOrEmpty()) return@ui toDevice(c, items, null)
                val q = when (c.what) {
                    Commands.What.THIS -> "Which file?"
                    Commands.What.NAMED -> "I couldn't find “${c.name}”"
                    else -> "I couldn't find a ${c.what.name.lowercase()}"
                }
                val sub = if (c.what != Commands.What.THIS && !SharedFolders.canReadMedia(this))
                    "Allow LAN Share to see photos in its Settings, or choose a file." else "Choose one:"
                val opts = mutableListOf<Option>()
                photoItem?.firstOrNull()?.let { f -> opts += Option("Latest photo", R.drawable.ic_image, sub = f.name) { toDevice(c, photoItem, null) } }
                shotItem?.firstOrNull()?.let { f -> opts += Option("Latest screenshot", R.drawable.ic_image, sub = f.name) { toDevice(c, shotItem, null) } }
                opts += Option("Choose a file…", R.drawable.ic_file) { pickFiles { toDevice(c, it, null) } }
                ask(q, sub, opts)
            }
        }
    }

    /** From the idle "Send a file…" button: files first, then where. */
    private fun handleItems(items: List<Client.Outgoing>) =
        toDevice(Commands.Command.Send(Commands.What.THIS, null, null, null), items, null)

    private fun toDevice(c: Commands.Command.Send, items: List<Client.Outgoing>, chosen: Device?) {
        val devices = DeviceStore.all(this)
        val d = chosen ?: c.device?.let { n -> devices.firstOrNull { it.name == n } } ?: devices.singleOrNull()
        val what = what(items)
        if (d == null) {
            ask("Send $what to which laptop?", "", devices.map { dev ->
                Option(dev.name, R.drawable.ic_laptop, chip = if (presence.status(dev.id).state == Presence.State.ONLINE) "Online" else null) {
                    toDevice(c, items, dev)
                }
            })
            return
        }
        say("Asking ${d.name} for its folders…", what)
        show(emptyList(), asking = false)
        worker.execute {
            val r = runCatching { Client.folders(DeviceStore.get(this, d.id) ?: d, myName) }
            ui {
                r.onFailure { e -> say("Couldn't reach ${d.name}", explain(e, d)); show(emptyList(), false) }
                r.onSuccess { (def, roots) ->
                    val label = Commands.findFolder(c.folder, roots.map { it.label })
                    val f = roots.firstOrNull { it.label == label }
                    when {
                        f != null -> confirmSend(d, f, items)
                        roots.isEmpty() -> say("${d.name} isn't sharing any folders", "Add one in LAN Share → Settings on the laptop.")
                        // The requirement: if no folder was named, always ask.
                        else -> ask(if (c.folder != null) "${d.name} has no “${c.folder}” folder. Which one?" else "Which folder on ${d.name}?",
                            what, roots.map { root ->
                                Option(root.label, folderIcon(root.key), chip = if (root.key == def) "Default" else null) { confirmSend(d, root, items) }
                            })
                    }
                }
            }
        }
    }

    private fun confirmSend(d: Device, f: Client.Folder, items: List<Client.Outgoing>) {
        Log.i(TAG, "assistant: plan send ${items.joinToString { it.name }} to ${d.name} in ${f.label}")
        countdown("Sending ${what(items)} to ${d.name}", "Into ${f.label} · ${formatSize(items.sumOf { it.size })}", "Send now") {
            Actions.send(this, d, f, "", f.label, items) { explain(it, d) }
        }
    }

    // ------------------------------------------------------------------ getting

    private fun planGet(c: Commands.Command.Get, chosen: Device? = null) {
        val devices = DeviceStore.all(this)
        val d = chosen ?: c.device?.let { n -> devices.firstOrNull { it.name == n } } ?: devices.singleOrNull()
        if (d == null) {
            ask("Get “${c.name}” from which laptop?", "", devices.map { dev -> Option(dev.name, R.drawable.ic_laptop) { planGet(c, dev) } })
            return
        }
        say("Looking for “${c.name}” on ${d.name}…")
        show(emptyList(), asking = false)
        worker.execute {
            // Searches the top of each shared folder on the laptop.
            val found = mutableListOf<Triple<Client.Folder, Client.RemoteFile, Int>>()
            val r = runCatching {
                val dev = DeviceStore.get(this, d.id) ?: d
                for (root in Client.folders(dev, myName).second) {
                    runCatching { Client.listing(dev, myName, root.key, "").second }.getOrDefault(emptyList()).forEach { f ->
                        val s = Commands.nameScore(f.name, c.name)
                        if (s > 0) found += Triple(root, f, s)
                    }
                }
            }
            val best = found.sortedWith(compareByDescending<Triple<Client.Folder, Client.RemoteFile, Int>> { it.third }.thenByDescending { it.second.modified })
            Log.i(TAG, "assistant: search \"${c.name}\" on ${d.name}: ${best.size} matches")
            ui {
                when {
                    r.isFailure -> { say("Couldn't reach ${d.name}", explain(r.exceptionOrNull()!!, d)); show(emptyList(), false) }
                    best.isEmpty() -> { say("I couldn't find “${c.name}” on ${d.name}", "I look at the top of each folder it shares. Tap ↓ next to it on the home screen to browse."); show(emptyList(), false) }
                    best.size == 1 || best[0].third > best[1].third -> confirmGet(d, best[0].first, best[0].second)
                    else -> ask("Which one?", "Found ${best.size} on ${d.name}", best.take(5).map { (root, f, _) ->
                        Option(f.name, fileIcon(f.name), sub = "${root.label} · ${formatSize(f.size)}") { confirmGet(d, root, f) }
                    })
                }
            }
        }
    }

    private fun confirmGet(d: Device, root: Client.Folder, f: Client.RemoteFile) {
        Log.i(TAG, "assistant: plan get ${f.name} from ${d.name} in ${root.label}")
        countdown("Getting ${f.name} from ${d.name}", "From ${root.label} · ${formatSize(f.size)} · into Downloads", "Get now") {
            Actions.fetch(this, d, root, "", listOf(f.name), f.size) { explain(it, d) }
        }
    }

    // ------------------------------------------------------------------ confirm and follow progress

    /** Shows what's about to happen for 2 seconds (time to cancel), then does it. */
    private fun countdown(title: String, sub: String, now: String, go: () -> Unit) {
        cancelCountdown()
        say(title, sub)
        val fire = Runnable {
            cancelCountdown()
            Log.i(TAG, "assistant: go")
            watchFrom = System.currentTimeMillis()
            closeWhenDone = true
            show(emptyList(), asking = false)
            go()
            showProgress()
        }
        countdown = fire
        bar.visibility = View.VISIBLE
        bar.progress = 0
        countdownAnim = ValueAnimator.ofInt(0, 1000).apply {
            duration = 2000
            addUpdateListener { bar.progress = it.animatedValue as Int }
            start()
        }
        main.postDelayed(fire, 2000)
        show(listOf(Option(now, R.drawable.ic_check_circle) { fire.run() }, Option("Cancel", R.drawable.ic_close) { idle() }), asking = true)
    }

    private fun cancelCountdown() {
        countdown?.let { main.removeCallbacks(it) }
        countdown = null
        countdownAnim?.cancel()
        countdownAnim = null
    }

    private fun showProgress() {
        if (watchFrom == 0L || countdown != null) return
        val t = Hub.visibleTransfers().firstOrNull { it.started >= watchFrom - 100 } ?: return
        val verb = if (t.inbound) "Getting" else "Sending"
        when (t.state) {
            Hub.State.ASKING -> say("Waiting for ${t.device}…", "")
            Hub.State.RUNNING -> {
                say("$verb ${t.name}…", "${formatSize(t.sent)} of ${formatSize(t.total)}" + if (t.count > 1) " · file ${t.index} of ${t.count}" else "")
                bar.visibility = View.VISIBLE
                bar.progress = if (t.total > 0) (t.sent * 1000 / t.total).toInt() else 0
                if (options.isEmpty()) show(listOf(Option("Cancel", R.drawable.ic_close) { Actions.worker.execute { t.cancel?.invoke() } }), asking = false)
            }
            Hub.State.DONE -> {
                bar.visibility = View.GONE
                val what = if (t.files > 1) plural(t.files, "file") else t.name
                say(if (t.inbound) "Got $what ✓" else "Sent $what ✓",
                    "${formatSize(t.total)} in ${formatDuration(t.elapsed)}" + if (t.folder.isNotEmpty()) " · in ${t.folder}" else "")
                show(emptyList(), asking = false)
                if (closeWhenDone) { closeWhenDone = false; main.postDelayed({ if (!isFinishing) finish() }, 2500) }
            }
            Hub.State.ERROR, Hub.State.CANCELLED -> {
                bar.visibility = View.GONE
                closeWhenDone = false
                say(if (t.state == Hub.State.CANCELLED) "Cancelled" else "That didn't work", t.message)
                show(listOfNotNull(t.retry?.let { r -> Option("Try again", R.drawable.ic_send) { Hub.dismiss(t.id); watchFrom = System.currentTimeMillis(); closeWhenDone = true; r() } }), asking = false)
            }
        }
    }

    // ------------------------------------------------------------------ files and speech

    private fun pickFiles(then: (List<Client.Outgoing>) -> Unit) {
        picking = then
        @Suppress("DEPRECATION")
        startActivityForResult(Intent(Intent.ACTION_OPEN_DOCUMENT).apply {
            addCategory(Intent.CATEGORY_OPENABLE)
            type = "*/*"
            putExtra(Intent.EXTRA_ALLOW_MULTIPLE, true)
        }, 1)
    }

    @Deprecated("Activity result API needs AndroidX; the plain framework keeps the app small")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        @Suppress("DEPRECATION")
        super.onActivityResult(requestCode, resultCode, data)
        val then = picking ?: return
        picking = null
        if (resultCode != RESULT_OK || data == null) return idle()
        val uris = mutableListOf<Uri>()
        data.clipData?.let { clip -> for (i in 0 until clip.itemCount) uris += clip.getItemAt(i).uri }
        if (uris.isEmpty()) data.data?.let { uris += it }
        worker.execute {
            val items = runCatching { Client.resolveFiles(this, uris) }.getOrNull()
            ui { if (items.isNullOrEmpty()) say("Couldn't read that file") else then(items) }
        }
    }

    private fun listen() {
        if (checkSelfPermission(Manifest.permission.RECORD_AUDIO) != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(arrayOf(Manifest.permission.RECORD_AUDIO), 2)
            return
        }
        if (!SpeechRecognizer.isRecognitionAvailable(this)) {
            say("Speech isn't available on this phone", "Type the command instead.")
            return
        }
        val r = recognizer ?: SpeechRecognizer.createSpeechRecognizer(this).also { recognizer = it; it.setRecognitionListener(Listener()) }
        val i = Intent(RecognizerIntent.ACTION_RECOGNIZE_SPEECH)
            .putExtra(RecognizerIntent.EXTRA_LANGUAGE_MODEL, RecognizerIntent.LANGUAGE_MODEL_FREE_FORM)
            .putExtra(RecognizerIntent.EXTRA_PARTIAL_RESULTS, true)
            .putExtra(RecognizerIntent.EXTRA_MAX_RESULTS, 3)
            .putExtra(RecognizerIntent.EXTRA_PREFER_OFFLINE, preferOffline)
        listening = true
        cancelCountdown()
        detail.visibility = View.VISIBLE
        detail.text = "Listening…"
        pulse = ObjectAnimator.ofFloat(mic, View.ALPHA, 1f, 0.55f).apply {
            duration = 500; repeatCount = ValueAnimator.INFINITE; repeatMode = ValueAnimator.REVERSE; start()
        }
        r.startListening(i)
        Log.i(TAG, "assistant: listening (offline first: $preferOffline)")
    }

    private fun stopListening() {
        if (listening) runCatching { recognizer?.stopListening() }
        listening = false
        pulse?.cancel()
        mic.alpha = 1f
    }

    override fun onRequestPermissionsResult(requestCode: Int, permissions: Array<out String>, grantResults: IntArray) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        if (requestCode == 2) {
            if (grantResults.firstOrNull() == PackageManager.PERMISSION_GRANTED) listen()
            else say("I need the microphone to hear you", "You can type the command instead.")
        }
    }

    private inner class Listener : RecognitionListener {
        override fun onReadyForSpeech(params: Bundle?) {}
        override fun onBeginningOfSpeech() {}
        override fun onRmsChanged(rmsdB: Float) {}
        override fun onBufferReceived(buffer: ByteArray?) {}
        override fun onEndOfSpeech() { stopListening() }
        override fun onEvent(eventType: Int, params: Bundle?) {}

        override fun onPartialResults(partial: Bundle?) {
            partial?.getStringArrayList(SpeechRecognizer.RESULTS_RECOGNITION)?.firstOrNull()?.let { input.setText(it) }
        }

        override fun onResults(results: Bundle?) {
            stopListening()
            val heard = results?.getStringArrayList(SpeechRecognizer.RESULTS_RECOGNITION).orEmpty()
            // Of the recognizer's guesses, take the first that makes sense as a command.
            val names = DeviceStore.all(this@AssistantActivity).map { it.name }
            val text = heard.firstOrNull { asking || Commands.parse(it, names) != Commands.Command.Unknown } ?: heard.firstOrNull()
            if (text.isNullOrBlank()) say("I didn't catch that", "Tap the mic and try again.") else handle(text)
        }

        override fun onError(error: Int) {
            stopListening()
            Log.i(TAG, "assistant: speech error $error")
            val noOfflineModel = error == SpeechRecognizer.ERROR_LANGUAGE_UNAVAILABLE ||
                error == SpeechRecognizer.ERROR_LANGUAGE_NOT_SUPPORTED || error == SpeechRecognizer.ERROR_SERVER_DISCONNECTED
            if (preferOffline && noOfflineModel) {
                preferOffline = false // hybrid: no offline model for this language, use the online recognizer
                listen()
                return
            }
            detail.visibility = View.VISIBLE
            detail.text = when (error) {
                SpeechRecognizer.ERROR_NO_MATCH, SpeechRecognizer.ERROR_SPEECH_TIMEOUT -> "I didn't catch that. Tap the mic and try again."
                SpeechRecognizer.ERROR_NETWORK, SpeechRecognizer.ERROR_NETWORK_TIMEOUT -> "Speech needs the internet on this phone. Type the command instead."
                SpeechRecognizer.ERROR_INSUFFICIENT_PERMISSIONS -> "I need the microphone to hear you."
                else -> "Speech stopped (error $error). Tap the mic to try again."
            }
        }
    }

    // ------------------------------------------------------------------ helpers

    private fun what(items: List<Client.Outgoing>) = items.singleOrNull()?.name ?: plural(items.size, "file")

    private fun explain(e: Throwable, d: Device): String = when (e) {
        is ConnectException, is SocketTimeoutException, is NoRouteToHostException ->
            "Is LAN Share running on ${d.name}, on the same Wi-Fi?"
        is SSLException -> "${d.name} didn't recognise this phone. Pair it again."
        is IOException -> "The connection dropped (${e.message}). Try again."
        else -> e.message ?: e.toString()
    }

    private fun dp(v: Int) = (v * resources.displayMetrics.density).toInt()

    companion object {
        private const val TAG = "LanShare"
    }
}
