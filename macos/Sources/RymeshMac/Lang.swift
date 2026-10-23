import Foundation

/// The interface language. The page and the Go console share one catalog,
/// web/i18n/en.json (copied into the app's Resources): the Chinese text is
/// the message id and English is looked up. The choice made in Settings is
/// the console's config.json "language"; empty follows the system.
enum Lang {
    /// "zh", "en", or "" for the system's language.
    static var choice = ""

    static var isEnglish: Bool {
        let tag = choice.isEmpty ? (Locale.preferredLanguages.first ?? "en") : choice
        return !tag.lowercased().hasPrefix("zh")
    }

    /// The system's language as the page should see it (the web view's own
    /// navigator.language follows the app bundle, not the system).
    static var system: String { Locale.preferredLanguages.first ?? "en" }

    private static let catalog: [String: String] = {
        guard let url = Bundle.main.url(forResource: "en", withExtension: "json"),
              let data = try? Data(contentsOf: url),
              let obj = try? JSONSerialization.jsonObject(with: data) as? [String: String] else { return [:] }
        return obj
    }()

    static func t(_ s: String) -> String {
        guard isEnglish, let v = catalog[s], !v.isEmpty else { return s }
        return v
    }
}

/// The app's own text in the interface language.
func L(_ s: String) -> String { Lang.t(s) }
