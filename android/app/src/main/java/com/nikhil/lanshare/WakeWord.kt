package com.nikhil.lanshare

import android.Manifest
import android.annotation.SuppressLint
import android.content.Context
import android.content.pm.PackageManager
import android.media.AudioFormat
import android.media.AudioRecord
import android.media.MediaRecorder
import android.util.Log
import org.tensorflow.lite.Interpreter
import java.io.FileInputStream
import java.nio.MappedByteBuffer
import java.nio.channels.FileChannel

/**
 * "Hey Jarvis" detector: openWakeWord's models (free, open source) run on the phone with
 * TensorFlow Lite. Nothing leaves the phone.
 *
 * Every 80 ms of 16 kHz audio (1280 samples):
 *  1. mel spectrogram of the last 1760 samples → 8 frames of 32 values (scaled x/10 + 2)
 *  2. the last 76 frames → speech embedding (96 values)
 *  3. the last 16 embeddings → "hey jarvis" score from 0 to 1
 * Same steps and sizes as openWakeWord's own streaming code (checked against it with real
 * and synthetic recordings; see .github/testdata).
 */
class WakeWord(ctx: Context) : AutoCloseable {
    private val mel = Interpreter(asset(ctx, "melspectrogram.tflite"))
    private val emb = Interpreter(asset(ctx, "embedding_model.tflite"))
    private val ww = Interpreter(asset(ctx, "hey_jarvis.tflite"))

    private val raw = ShortArray(MEL_INPUT)                // last 1760 samples (silence at first)
    private val mels = ArrayDeque<FloatArray>()            // mel frames, newest last
    private val feats = ArrayDeque<FloatArray>()           // embeddings, newest last
    private var chunks = 0

    private val melIn = arrayOf(FloatArray(MEL_INPUT))
    private val melOut = arrayOf(arrayOf(Array(8) { FloatArray(32) }))
    private val embIn = arrayOf(Array(76) { Array(32) { FloatArray(1) } })
    private val embOut = arrayOf(arrayOf(arrayOf(FloatArray(96))))
    private val wwIn = arrayOf(Array(16) { FloatArray(96) })
    private val wwOut = arrayOf(FloatArray(1))

    init {
        mel.resizeInput(0, intArrayOf(1, MEL_INPUT))
        mel.allocateTensors()
        repeat(76) { mels.addLast(FloatArray(32) { 1f }) } // as openWakeWord starts
    }

    /** Feeds exactly 1280 samples (80 ms); returns the score for "hey jarvis", 0..1. */
    fun chunk(pcm: ShortArray): Float {
        require(pcm.size == CHUNK)
        System.arraycopy(raw, CHUNK, raw, 0, MEL_INPUT - CHUNK)
        System.arraycopy(pcm, 0, raw, MEL_INPUT - CHUNK, CHUNK)
        for (i in 0 until MEL_INPUT) melIn[0][i] = raw[i].toFloat()
        mel.run(melIn, melOut)
        for (f in melOut[0][0]) {
            mels.addLast(FloatArray(32) { f[it] / 10f + 2f })
            if (mels.size > 120) mels.removeFirst()
        }
        val start = mels.size - 76
        for (t in 0 until 76) { val row = mels[start + t]; for (k in 0 until 32) embIn[0][t][k][0] = row[k] }
        emb.run(embIn, embOut)
        feats.addLast(embOut[0][0][0].copyOf())
        if (feats.size > 16) feats.removeFirst()
        chunks++
        if (feats.size < 16 || chunks < 5) return 0f
        for (t in 0 until 16) System.arraycopy(feats[t], 0, wwIn[0][t], 0, 96)
        ww.run(wwIn, wwOut)
        return wwOut[0][0]
    }

    override fun close() { mel.close(); emb.close(); ww.close() }

    companion object {
        const val CHUNK = 1280
        const val MEL_INPUT = 1760
        const val THRESHOLD = 0.5f

        private fun asset(ctx: Context, name: String): MappedByteBuffer =
            ctx.assets.openFd("wakeword/$name").use { fd ->
                FileInputStream(fd.fileDescriptor).channel.map(FileChannel.MapMode.READ_ONLY, fd.startOffset, fd.declaredLength)
            }

        fun enabled(ctx: Context) = ctx.getSharedPreferences("settings", Context.MODE_PRIVATE).getBoolean("wake", false)
        fun setEnabled(ctx: Context, on: Boolean) =
            ctx.getSharedPreferences("settings", Context.MODE_PRIVATE).edit().putBoolean("wake", on).apply()

        fun canListen(ctx: Context) = enabled(ctx) &&
            ctx.checkSelfPermission(Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED

        /** Runs a 16 kHz mono 16-bit WAV through the detector (tests). Returns the highest score. */
        fun scoreWav(ctx: Context, bytes: ByteArray): Float {
            val data = bytes.copyOfRange(44, bytes.size) // canonical WAV header
            val samples = ShortArray(data.size / 2) { ((data[2 * it + 1].toInt() shl 8) or (data[2 * it].toInt() and 0xff)).toShort() }
            val padded = ShortArray(16000) + samples + ShortArray(16000)
            var best = 0f
            WakeWord(ctx).use { w ->
                var i = 0
                while (i + CHUNK <= padded.size) { best = maxOf(best, w.chunk(padded.copyOfRange(i, i + CHUNK))); i += CHUNK }
            }
            return best
        }
    }
}

/**
 * Listens on the microphone for "Hey Jarvis" in the background (inside the receiving
 * service). Steps aside while the assistant uses the microphone itself.
 */
class WakeListener(private val ctx: Context, private val onWake: () -> Unit) {
    @Volatile private var running = false
    @Volatile var paused = false
    private var thread: Thread? = null

    fun start() {
        if (running) return
        running = true
        thread = Thread({ loop() }, "wake-word").apply { isDaemon = true; start() }
    }

    fun stop() {
        running = false
        thread?.interrupt()
        thread = null
    }

    @SuppressLint("MissingPermission") // checked by WakeWord.canListen before starting
    private fun loop() {
        val detector = runCatching { WakeWord(ctx) }.getOrElse {
            Log.w("LanShare", "wake word: models didn't load: $it"); running = false; return
        }
        val pcm = ShortArray(WakeWord.CHUNK)
        var lastWake = 0L
        try { detector.use {
            while (running) {
                if (paused) { Thread.sleep(300); continue }
                val min = AudioRecord.getMinBufferSize(16000, AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT)
                val rec = runCatching {
                    AudioRecord(MediaRecorder.AudioSource.VOICE_RECOGNITION, 16000, AudioFormat.CHANNEL_IN_MONO,
                        AudioFormat.ENCODING_PCM_16BIT, maxOf(min, WakeWord.CHUNK * 4))
                }.getOrNull()
                if (rec == null || rec.state != AudioRecord.STATE_INITIALIZED) {
                    Log.w("LanShare", "wake word: microphone unavailable")
                    rec?.release()
                    Thread.sleep(10_000)
                    continue
                }
                Log.i("LanShare", "wake word: listening")
                rec.startRecording()
                try {
                    while (running && !paused) {
                        var got = 0
                        while (got < pcm.size && running) {
                            val n = rec.read(pcm, got, pcm.size - got)
                            if (n <= 0) break
                            got += n
                        }
                        if (got < pcm.size) break
                        val score = it.chunk(pcm)
                        val now = System.currentTimeMillis()
                        if (score > WakeWord.THRESHOLD && now - lastWake > 2_000) {
                            lastWake = now
                            Log.i("LanShare", "wake word heard (score ${"%.2f".format(score)})")
                            paused = true // let the assistant have the microphone
                            onWake()
                        }
                    }
                } finally {
                    runCatching { rec.stop() }
                    rec.release()
                }
            }
        } } catch (e: InterruptedException) {
            // stopped
        }
    }
}
