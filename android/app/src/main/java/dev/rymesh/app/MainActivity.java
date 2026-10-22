package dev.rymesh.app;

import android.app.Activity;
import android.content.ContentResolver;
import android.content.ContentValues;
import android.content.Intent;
import android.content.res.AssetManager;
import android.content.res.Configuration;
import android.graphics.Insets;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Environment;
import android.provider.MediaStore;
import android.view.Gravity;
import android.view.View;
import android.view.WindowInsets;
import android.view.WindowInsetsController;
import android.webkit.CookieManager;
import android.webkit.JavascriptInterface;
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.FrameLayout;
import android.widget.ProgressBar;
import android.widget.Toast;

import java.io.File;
import java.io.FileOutputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;
import java.security.KeyStore;
import java.security.SecureRandom;
import java.util.Arrays;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;

import javax.crypto.Cipher;
import javax.crypto.KeyGenerator;
import javax.crypto.SecretKey;
import javax.crypto.spec.GCMParameterSpec;

import android.security.keystore.KeyGenParameterSpec;
import android.security.keystore.KeyProperties;

import org.json.JSONObject;

import dev.rymesh.mobile.Mobile;

/**
 * One WebView over the embedded Go console (package mobile). The console is a
 * hub: it reaches the user's servers over SSH and runs no agents itself.
 *
 * The page asks this shell, through RymeshNative, for what a web page
 * cannot do well on a phone: the share sheet, saving a file, downloading one
 * into Downloads, and matching the system bars to its theme.
 */
public class MainActivity extends Activity {
    private static final int REQ_SAVE = 1, REQ_OPEN = 2;

    private WebView web;
    private FrameLayout root;
    private ProgressBar loading;
    private String pendingSave;                  // file text waiting for a destination
    private ValueCallback<Uri[]> pendingChooser; // <input type=file> waiting for a pick
    private volatile Uri console;                // the embedded console's origin
    private final Map<String, Uri> downloads = new ConcurrentHashMap<>(); // finished, by page id

    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        if ((getApplicationInfo().flags & android.content.pm.ApplicationInfo.FLAG_DEBUGGABLE) != 0) {
            WebView.setWebContentsDebuggingEnabled(true);
        }
        web = new KeyboardKeepingWebView(this);
        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        web.addJavascriptInterface(new Bridge(), "RymeshNative");
        web.setWebViewClient(new WebViewClient() {
            // Only the console itself loads here, where the page has the native
            // bridge. Everything else (docs, URLs in answers, other local
            // services such as a dev server on another port) opens outside.
            @Override
            public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest req) {
                Uri u = req.getUrl();
                Uri c = console;
                if (c != null && c.getScheme().equals(u.getScheme()) && c.getHost().equals(u.getHost())
                        && c.getPort() == u.getPort()) return false;
                try {
                    startActivity(new Intent(Intent.ACTION_VIEW, u));
                } catch (android.content.ActivityNotFoundException e) {
                    Toast.makeText(MainActivity.this, "没有可以打开此链接的应用", Toast.LENGTH_SHORT).show();
                }
                return true;
            }

            @Override
            public void onPageFinished(WebView view, String url) {
                loading.setVisibility(View.GONE);
            }
        });
        web.setWebChromeClient(new WebChromeClient() {
            @Override
            public boolean onShowFileChooser(WebView view, ValueCallback<Uri[]> cb, FileChooserParams params) {
                if (pendingChooser != null) pendingChooser.onReceiveValue(null);
                pendingChooser = cb;
                Intent i = new Intent(Intent.ACTION_OPEN_DOCUMENT).addCategory(Intent.CATEGORY_OPENABLE).setType("*/*");
                try {
                    startActivityForResult(i, REQ_OPEN);
                } catch (Exception e) {
                    pendingChooser = null;
                    return false;
                }
                return true;
            }
        });

        // Android 15+ draws edge to edge. The container takes the system bars,
        // the display cutout and the keyboard as padding, and consumes them:
        // otherwise the page adds the cutout again through
        // env(safe-area-inset-top) and the top bar sits far below the status bar.
        root = new FrameLayout(this);
        root.addView(web);
        loading = new ProgressBar(this);
        root.addView(loading, new FrameLayout.LayoutParams(dp(36), dp(36), Gravity.CENTER));
        root.setOnApplyWindowInsetsListener((View v, WindowInsets in) -> {
            if (Build.VERSION.SDK_INT >= 30) {
                Insets i = in.getInsets(WindowInsets.Type.systemBars() | WindowInsets.Type.displayCutout()
                        | WindowInsets.Type.ime());
                v.setPadding(i.left, i.top, i.right, i.bottom);
                return WindowInsets.CONSUMED;
            }
            v.setPadding(in.getSystemWindowInsetLeft(), in.getSystemWindowInsetTop(),
                    in.getSystemWindowInsetRight(), in.getSystemWindowInsetBottom());
            return in.consumeSystemWindowInsets();
        });
        setContentView(root);
        if (Build.VERSION.SDK_INT >= 30) getWindow().setDecorFitsSystemWindows(false);
        applyBars(isNight());

        // Always (re)start the console and load a fresh login URL, also when
        // Android recreates the activity: after the process was killed in the
        // background a restored page pointed at a console that no longer ran,
        // and showed a blank screen. Within a live process Mobile.start only
        // hands out a new URL for the running console.
        new Thread(() -> {
            try {
                File files = getFilesDir();
                installNodeBinaries(new File(files, "data/dist"));
                String url = Mobile.start(files.getAbsolutePath(), vaultKey());
                // The port only, never the ticket: logcat is readable over adb.
                console = Uri.parse(url);
                android.util.Log.i("Rymesh", "console on " + console.getPort());
                runOnUiThread(() -> web.loadUrl(url));
            } catch (Exception e) {
                String html = "<p style='font:16px sans-serif;padding:24px'>启动失败：" + e.getMessage() + "</p>";
                runOnUiThread(() -> web.loadData(html, "text/html; charset=utf-8", "utf-8"));
            }
        }).start();
    }

    /**
     * A WebView hides the keyboard when its window loses focus. Long-pressing
     * the keyboard switch (the globe) opens the system's input-method picker,
     * which takes window focus: the keyboard closed and the picker went with
     * it, so the input method could not be changed. While the keyboard is up,
     * that focus loss is not passed on.
     */
    private static class KeyboardKeepingWebView extends WebView {
        KeyboardKeepingWebView(android.content.Context c) { super(c); }

        @Override
        public void onWindowFocusChanged(boolean hasWindowFocus) {
            if (!hasWindowFocus && Build.VERSION.SDK_INT >= 30) {
                WindowInsets in = getRootWindowInsets();
                if (in != null && in.isVisible(WindowInsets.Type.ime())) return;
            }
            super.onWindowFocusChanged(hasWindowFocus);
        }
    }

    private int dp(int v) { return Math.round(v * getResources().getDisplayMetrics().density); }

    private boolean isNight() {
        return (getResources().getConfiguration().uiMode & Configuration.UI_MODE_NIGHT_MASK) == Configuration.UI_MODE_NIGHT_YES;
    }

    /** Colours the area behind the system bars and picks light or dark bar icons. */
    private void applyBars(boolean dark) {
        int bg = dark ? 0xFF0B0B0A : 0xFFFCFCFB; // --bg in web/styles.css
        root.setBackgroundColor(bg);
        web.setBackgroundColor(bg);
        getWindow().getDecorView().setBackgroundColor(bg);
        if (Build.VERSION.SDK_INT >= 30) {
            WindowInsetsController c = getWindow().getInsetsController();
            if (c != null) {
                int light = WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS | WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS;
                c.setSystemBarsAppearance(dark ? 0 : light, light);
            }
        } else {
            View d = getWindow().getDecorView();
            int f = d.getSystemUiVisibility();
            int light = View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR | View.SYSTEM_UI_FLAG_LIGHT_NAVIGATION_BAR;
            d.setSystemUiVisibility(dark ? f & ~light : f | light);
        }
    }

    /** Methods the page calls as window.RymeshNative.*; they run on a binder thread. */
    private class Bridge {
        @JavascriptInterface
        public void share(String text, String title) {
            runOnUiThread(() -> {
                Intent i = new Intent(Intent.ACTION_SEND).setType("text/plain").putExtra(Intent.EXTRA_TEXT, text);
                if (title != null && !title.isEmpty()) i.putExtra(Intent.EXTRA_SUBJECT, title);
                startActivity(Intent.createChooser(i, title == null || title.isEmpty() ? "分享" : title));
            });
        }

        @JavascriptInterface
        public void saveFile(String name, String text) {
            runOnUiThread(() -> {
                pendingSave = text;
                Intent i = new Intent(Intent.ACTION_CREATE_DOCUMENT).addCategory(Intent.CATEGORY_OPENABLE)
                        .setType("application/octet-stream").putExtra(Intent.EXTRA_TITLE, name);
                startActivityForResult(i, REQ_SAVE);
            });
        }

        /**
         * Downloads a file from the console (a path such as
         * /n/server/api/files/download?path=…) into Download/Rymesh, reporting
         * progress to the page's window.acDownload(id, done, total, state, info).
         */
        @JavascriptInterface
        public void download(String id, String path, String name) {
            new Thread(() -> runDownload(id, path, name), "download").start();
        }

        /** Opens a finished download with whatever app handles its type. */
        @JavascriptInterface
        public void openDownload(String id) {
            Uri uri = downloads.get(id);
            if (uri == null) return;
            runOnUiThread(() -> {
                Intent i = new Intent(Intent.ACTION_VIEW).setDataAndType(uri, getContentResolver().getType(uri))
                        .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION);
                try {
                    startActivity(Intent.createChooser(i, "打开"));
                } catch (android.content.ActivityNotFoundException e) {
                    Toast.makeText(MainActivity.this, "没有可以打开此文件的应用", Toast.LENGTH_SHORT).show();
                }
            });
        }

        @JavascriptInterface
        public void setTheme(boolean dark) {
            runOnUiThread(() -> applyBars(dark));
        }
    }

    private void reportDownload(String id, long done, long total, String state, String info) {
        String js = "window.acDownload&&acDownload(" + JSONObject.quote(id) + "," + done + "," + total + ","
                + (state == null ? "null" : JSONObject.quote(state)) + ","
                + (info == null ? "null" : JSONObject.quote(info)) + ")";
        runOnUiThread(() -> web.evaluateJavascript(js, null));
    }

    // The console is on loopback in this process; the request carries the
    // page's login cookie, so it sees exactly what the page may see.
    private void runDownload(String id, String path, String name) {
        Uri c = console;
        if (c == null || path == null || !path.startsWith("/")) {
            reportDownload(id, 0, 0, "error", "地址无效");
            return;
        }
        ContentResolver cr = getContentResolver();
        Uri item = null;
        File file = null;
        HttpURLConnection conn = null;
        try {
            String url = c.getScheme() + "://" + c.getHost() + ":" + c.getPort() + path;
            conn = (HttpURLConnection) new URL(url).openConnection();
            String cookie = CookieManager.getInstance().getCookie(url);
            if (cookie != null) conn.setRequestProperty("Cookie", cookie);
            conn.setRequestProperty("Accept-Encoding", "identity");
            conn.setConnectTimeout(15000);
            conn.setReadTimeout(120000);
            int code = conn.getResponseCode();
            if (code != 200) {
                String msg = "HTTP " + code;
                try (InputStream es = conn.getErrorStream()) {
                    if (es != null) {
                        java.io.ByteArrayOutputStream body = new java.io.ByteArrayOutputStream();
                        byte[] b = new byte[4096];
                        for (int n; (n = es.read(b)) > 0 && body.size() < 64 * 1024; ) body.write(b, 0, n);
                        msg = new JSONObject(body.toString("UTF-8")).optString("error", msg);
                    }
                } catch (Exception ignored) {
                }
                reportDownload(id, 0, 0, "error", msg);
                return;
            }
            long total = conn.getContentLengthLong();
            String type = conn.getContentType();
            OutputStream os;
            String where;
            if (Build.VERSION.SDK_INT >= 29) {
                // Downloads needs no storage permission from Android 10 on; a
                // taken name gets " (1)" from the media store itself.
                ContentValues v = new ContentValues();
                v.put(MediaStore.Downloads.DISPLAY_NAME, name);
                v.put(MediaStore.Downloads.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS + "/Rymesh");
                if (type != null) v.put(MediaStore.Downloads.MIME_TYPE, type.split(";")[0].trim());
                v.put(MediaStore.Downloads.IS_PENDING, 1);
                item = cr.insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, v);
                if (item == null) throw new java.io.IOException("无法在“下载”中创建文件");
                os = cr.openOutputStream(item);
                where = "下载/Rymesh";
            } else {
                // Older Android: the app's own download folder, no permission.
                File dir = new File(getExternalFilesDir(Environment.DIRECTORY_DOWNLOADS), "Rymesh");
                dir.mkdirs();
                file = new File(dir, name);
                for (int i = 1; file.exists(); i++) {
                    int dot = name.lastIndexOf('.');
                    file = new File(dir, dot > 0 ? name.substring(0, dot) + " (" + i + ")" + name.substring(dot) : name + " (" + i + ")");
                }
                os = new FileOutputStream(file);
                where = dir.getAbsolutePath();
            }
            long done = 0, last = 0;
            try (InputStream in = conn.getInputStream(); OutputStream out = os) {
                byte[] buf = new byte[64 * 1024];
                for (int n; (n = in.read(buf)) > 0; ) {
                    out.write(buf, 0, n);
                    done += n;
                    long now = System.currentTimeMillis();
                    if (now - last > 250) {
                        last = now;
                        reportDownload(id, done, total, null, null);
                    }
                }
            }
            if (total >= 0 && done != total) throw new java.io.IOException("连接中断");
            if (item != null) {
                ContentValues v = new ContentValues();
                v.put(MediaStore.Downloads.IS_PENDING, 0);
                cr.update(item, v, null, null);
                downloads.put(id, item);
            }
            reportDownload(id, done, total, "done", where);
        } catch (Exception e) {
            if (item != null) cr.delete(item, null, null);
            if (file != null) file.delete();
            reportDownload(id, 0, 0, "error", e.getMessage() == null ? e.toString() : e.getMessage());
        } finally {
            if (conn != null) conn.disconnect();
        }
    }

    @Override
    protected void onActivityResult(int req, int result, Intent data) {
        super.onActivityResult(req, result, data);
        Uri uri = result == RESULT_OK && data != null ? data.getData() : null;
        if (req == REQ_OPEN) {
            if (pendingChooser != null) pendingChooser.onReceiveValue(uri == null ? null : new Uri[]{uri});
            pendingChooser = null;
        } else if (req == REQ_SAVE) {
            String text = pendingSave;
            pendingSave = null;
            if (uri == null || text == null) return;
            try (OutputStream os = getContentResolver().openOutputStream(uri, "wt")) {
                os.write(text.getBytes(StandardCharsets.UTF_8));
                Toast.makeText(this, "已保存", Toast.LENGTH_SHORT).show();
            } catch (Exception e) {
                Toast.makeText(this, "保存失败：" + e.getMessage(), Toast.LENGTH_LONG).show();
            }
        }
    }

    /**
     * The key that seals the SSH keys in the console's database (vaultseal.go).
     * It is kept wrapped by a key that never leaves the Android Keystore, so
     * the app's files alone do not reveal it. On failure the console falls
     * back to a key file of its own.
     */
    private byte[] vaultKey() {
        try {
            final String alias = "rymesh-vault";
            KeyStore ks = KeyStore.getInstance("AndroidKeyStore");
            ks.load(null);
            if (!ks.containsAlias(alias)) {
                KeyGenerator g = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore");
                g.init(new KeyGenParameterSpec.Builder(alias,
                        KeyProperties.PURPOSE_ENCRYPT | KeyProperties.PURPOSE_DECRYPT)
                        .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                        .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                        .setKeySize(256).build());
                g.generateKey();
            }
            SecretKey wrap = (SecretKey) ks.getKey(alias, null);
            File f = new File(getFilesDir(), "vault.key.enc");
            Cipher c = Cipher.getInstance("AES/GCM/NoPadding");
            if (f.exists()) {
                byte[] b = java.nio.file.Files.readAllBytes(f.toPath());
                c.init(Cipher.DECRYPT_MODE, wrap, new GCMParameterSpec(128, b, 0, 12));
                return c.doFinal(b, 12, b.length - 12);
            }
            byte[] key = new byte[32];
            new SecureRandom().nextBytes(key);
            c.init(Cipher.ENCRYPT_MODE, wrap);
            byte[] iv = c.getIV(), sealed = c.doFinal(key);
            byte[] out = Arrays.copyOf(iv, iv.length + sealed.length);
            System.arraycopy(sealed, 0, out, iv.length, sealed.length);
            File tmp = new File(getFilesDir(), "vault.key.enc.tmp");
            java.nio.file.Files.write(tmp.toPath(), out);
            if (!tmp.renameTo(f)) throw new java.io.IOException("rename failed");
            return key;
        } catch (Exception e) {
            android.util.Log.w("Rymesh", "vault key unavailable: " + e);
            return null;
        }
    }

    /**
     * Linux node programs (gzipped), uploaded when the user deploys to a
     * server. They are copied out of the APK once per installed version;
     * files an earlier version left (unpacked programs) are removed.
     */
    private void installNodeBinaries(File dir) throws Exception {
        String stamp = String.valueOf(getPackageManager().getPackageInfo(getPackageName(), 0).lastUpdateTime);
        File marker = new File(dir, ".installed");
        if (marker.exists() && stamp.equals(new String(java.nio.file.Files.readAllBytes(marker.toPath())))) return;
        AssetManager am = getAssets();
        String[] names = am.list("dist");
        if (names == null) return;
        dir.mkdirs();
        // In the APK they end in .gzip (a .gz asset would be stored
        // unpacked); the console looks for .gz.
        java.util.List<String> keep = new java.util.ArrayList<>();
        for (String name : names) keep.add(name.replaceAll("\\.gzip$", ".gz"));
        File[] old = dir.listFiles();
        if (old != null) for (File f : old) if (!keep.contains(f.getName())) f.delete();
        for (String name : names) {
            File dst = new File(dir, name.replaceAll("\\.gzip$", ".gz"));
            try (InputStream in = am.open("dist/" + name); OutputStream os = new FileOutputStream(dst)) {
                byte[] buf = new byte[1 << 16];
                for (int n; (n = in.read(buf)) > 0; ) os.write(buf, 0, n);
            }
        }
        java.nio.file.Files.write(marker.toPath(), stamp.getBytes());
    }

    // Back closes what is open in the page (picker, sheet, sidebar) or returns
    // to the home screen before it leaves the app. Android 13+ delivers it
    // through OnBackInvokedDispatcher (onBackPressed is not called when
    // targeting 36); leaving puts the app in the background like the system
    // default, so the console keeps its connections.
    private void back() {
        web.evaluateJavascript("window.acBack ? window.acBack() : false", v -> {
            if (!"true".equals(v)) moveTaskToBack(true);
        });
    }

    @Override
    public void onBackPressed() { back(); }

    @Override
    protected void onPostCreate(Bundle state) {
        super.onPostCreate(state);
        if (Build.VERSION.SDK_INT >= 33) {
            getOnBackInvokedDispatcher().registerOnBackInvokedCallback(
                    android.window.OnBackInvokedDispatcher.PRIORITY_DEFAULT, this::back);
        }
    }

    @Override
    protected void onDestroy() {
        if (isFinishing()) Mobile.stop();
        super.onDestroy();
    }
}
