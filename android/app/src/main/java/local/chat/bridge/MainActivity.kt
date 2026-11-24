package local.chat.bridge

import android.annotation.SuppressLint
import android.app.Activity
import android.app.AlertDialog
import android.content.ContentValues
import android.content.Intent
import android.graphics.Color
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Environment
import android.provider.MediaStore
import android.webkit.JsResult
import android.webkit.URLUtil
import android.webkit.ValueCallback
import android.webkit.WebChromeClient
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Toast
import java.io.File
import java.io.FileOutputStream
import java.net.HttpURLConnection
import java.net.URL

class MainActivity : Activity() {
    companion object {
        private const val FILE_CHOOSER_REQ = 1001
        private const val LOCAL_URL = "http://127.0.0.1:8787"
    }

    private var serverProcess: Process? = null
    private lateinit var webView: WebView
    private var filePathCallback: ValueCallback<Array<Uri>>? = null

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        window.statusBarColor = Color.parseColor("#181818")
        window.navigationBarColor = Color.parseColor("#181818")

        webView = WebView(this).apply {
            setBackgroundColor(Color.parseColor("#181818"))
            settings.javaScriptEnabled = true
            settings.domStorageEnabled = true
            settings.allowFileAccess = false
            settings.mediaPlaybackRequiresUserGesture = false

            webViewClient = object : WebViewClient() {
                override fun shouldOverrideUrlLoading(view: WebView?, request: WebResourceRequest?): Boolean {
                    val uri = request?.url ?: return false
                    if (uri.host == "127.0.0.1" || uri.host == "localhost") {
                        return false
                    }
                    // Open clicked external links in Android's default browser.
                    runCatching { startActivity(Intent(Intent.ACTION_VIEW, uri)) }
                    return true
                }
            }

            webChromeClient = object : WebChromeClient() {
                override fun onShowFileChooser(
                    view: WebView?,
                    callback: ValueCallback<Array<Uri>>?,
                    params: FileChooserParams?
                ): Boolean {
                    filePathCallback?.onReceiveValue(null)
                    filePathCallback = callback
                    val intent = Intent(Intent.ACTION_GET_CONTENT).apply {
                        addCategory(Intent.CATEGORY_OPENABLE)
                        type = "*/*"
                        putExtra(Intent.EXTRA_ALLOW_MULTIPLE, true)
                    }
                    return try {
                        startActivityForResult(Intent.createChooser(intent, "Attach file"), FILE_CHOOSER_REQ)
                        true
                    } catch (e: Exception) {
                        filePathCallback = null
                        Toast.makeText(this@MainActivity, "File picker unavailable", Toast.LENGTH_SHORT).show()
                        false
                    }
                }

                override fun onJsConfirm(
                    view: WebView?,
                    url: String?,
                    message: String?,
                    result: JsResult?
                ): Boolean {
                    AlertDialog.Builder(this@MainActivity)
                        .setTitle("LocalChat")
                        .setMessage(message ?: "")
                        .setPositiveButton("Confirm") { _, _ -> result?.confirm() }
                        .setNegativeButton("Cancel") { _, _ -> result?.cancel() }
                        .setOnCancelListener { result?.cancel() }
                        .show()
                    return true
                }
            }

            setDownloadListener { url, _, contentDisposition, mimeType, _ ->
                val filename = URLUtil.guessFileName(url, contentDisposition, mimeType)
                downloadAttachment(url, filename, mimeType ?: "application/octet-stream")
            }
        }
        setContentView(webView)

        Thread {
            val started = startLocalServer()
            runOnUiThread {
                if (started) {
                    webView.loadUrl(LOCAL_URL)
                } else {
                    Toast.makeText(this, "Failed to start local server", Toast.LENGTH_LONG).show()
                }
            }
        }.start()
    }

    private fun startLocalServer(): Boolean {
        if (isHealthy()) return true

        val bin = File(applicationInfo.nativeLibraryDir, "liblocalchat.so")
        if (!bin.exists()) return false

        val dataDir = File(filesDir, "LocalChat").apply { mkdirs() }
        val logFile = File(dataDir, "localchat.log")

        val pb = ProcessBuilder(
            bin.absolutePath,
            "serve",
            "--data", dataDir.absolutePath,
            "--outbound-only",
            "--auto-init",
            "--no-idle-exit"
        ).apply {
            environment()["GOMEMLIMIT"] = "64MiB"
            redirectErrorStream(true)
            redirectOutput(ProcessBuilder.Redirect.appendTo(logFile))
        }

        return try {
            serverProcess = pb.start()
            val deadline = System.currentTimeMillis() + 5000
            while (System.currentTimeMillis() < deadline) {
                if (isHealthy()) return true
                Thread.sleep(100)
            }
            false
        } catch (e: Exception) {
            false
        }
    }

    private fun isHealthy(): Boolean {
        return try {
            val conn = (URL("$LOCAL_URL/api/health").openConnection() as HttpURLConnection).apply {
                connectTimeout = 400
                readTimeout = 400
            }
            try {
                conn.responseCode == 200
            } finally {
                conn.disconnect()
            }
        } catch (_: Exception) {
            false
        }
    }

    private fun downloadAttachment(url: String, filename: String, mimeType: String) {
        Toast.makeText(this, "Saving $filename…", Toast.LENGTH_SHORT).show()
        Thread {
            try {
                val conn = URL(url).openConnection() as HttpURLConnection
                conn.connectTimeout = 5000
                conn.readTimeout = 60000
                conn.inputStream.use { input ->
                    if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
                        val values = ContentValues().apply {
                            put(MediaStore.Downloads.DISPLAY_NAME, filename)
                            put(MediaStore.Downloads.MIME_TYPE, mimeType)
                            put(MediaStore.Downloads.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS)
                        }
                        val uri = contentResolver.insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, values)
                            ?: throw IllegalStateException("MediaStore insert failed")
                        contentResolver.openOutputStream(uri)?.use { out -> input.copyTo(out) }
                    } else {
                        val dir = getExternalFilesDir(Environment.DIRECTORY_DOWNLOADS) ?: filesDir
                        FileOutputStream(File(dir, filename)).use { out -> input.copyTo(out) }
                    }
                }
                conn.disconnect()
                runOnUiThread {
                    Toast.makeText(this, "Saved to Downloads: $filename", Toast.LENGTH_SHORT).show()
                }
            } catch (e: Exception) {
                runOnUiThread {
                    Toast.makeText(this, "Download failed: ${e.message}", Toast.LENGTH_LONG).show()
                }
            }
        }.start()
    }

    @Deprecated("Deprecated in Java")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode != FILE_CHOOSER_REQ) return

        val results: Array<Uri>? = when {
            resultCode != RESULT_OK || data == null -> null
            data.clipData != null -> {
                val clip = data.clipData!!
                Array(clip.itemCount) { i -> clip.getItemAt(i).uri }
            }
            data.data != null -> arrayOf(data.data!!)
            else -> null
        }
        filePathCallback?.onReceiveValue(results)
        filePathCallback = null
    }

    @Deprecated("Deprecated in Java")
    override fun onBackPressed() {
        if (webView.canGoBack()) {
            webView.goBack()
        } else {
            super.onBackPressed()
        }
    }

    override fun onDestroy() {
        super.onDestroy()
        serverProcess?.destroy()
        serverProcess = null
    }
}
