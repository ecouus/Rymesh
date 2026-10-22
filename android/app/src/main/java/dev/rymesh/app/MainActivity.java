package dev.rymesh.app;

import android.app.Activity;
import android.content.Intent;
import android.content.res.AssetManager;
import android.content.res.Configuration;
import android.graphics.Insets;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.view.Gravity;
import android.view.View;
import android.view.WindowInsets;
import android.view.WindowInsetsController;
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
import java.nio.charset.StandardCharsets;

import dev.rymesh.mobile.Mobile;

/**
 * One WebView over the embedded Go console (package mobile). The console is a
 * hub: it reaches the user's servers over SSH and runs no agents itself.
 *
 * The page asks this shell, through RymeshNative, for what a web page
 * cannot do well on a phone: the share sheet, saving a file, and matching the
 * system bars to its theme.
 */
public class MainActivity extends Activity {
    private static final int REQ_SAVE = 1, REQ_OPEN = 2;

    private WebView web;
    private FrameLayout root;
    private ProgressBar loading;
    private String pendingSave;                  // file text waiting for a destination
    private ValueCallback<Uri[]> pendingChooser; // <input type=file> waiting for a pick

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
            // Links out of the console (docs, URLs in answers) open in the browser.
            @Override
            public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest req) {
                Uri u = req.getUrl();
                if ("127.0.0.1".equals(u.getHost())) return false;
                startActivity(new Intent(Intent.ACTION_VIEW, u));
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
                String url = Mobile.start(files.getAbsolutePath());
                // The port only, never the ticket: logcat is readable over adb.
                android.util.Log.i("Rymesh", "console on " + Uri.parse(url).getPort());
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
        int bg = dark ? 0xFF0F0F10 : 0xFFFFFFFF; // --bg in web/styles.css
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

        @JavascriptInterface
        public void setTheme(boolean dark) {
            runOnUiThread(() -> applyBars(dark));
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
     * Linux node programs, uploaded when the user deploys to a server. They
     * are copied out of the APK once per installed version.
     */
    private void installNodeBinaries(File dir) throws Exception {
        String stamp = String.valueOf(getPackageManager().getPackageInfo(getPackageName(), 0).lastUpdateTime);
        File marker = new File(dir, ".installed");
        if (marker.exists() && stamp.equals(new String(java.nio.file.Files.readAllBytes(marker.toPath())))) return;
        AssetManager am = getAssets();
        String[] names = am.list("dist");
        if (names == null) return;
        dir.mkdirs();
        for (String name : names) {
            try (InputStream in = am.open("dist/" + name); OutputStream os = new FileOutputStream(new File(dir, name))) {
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
