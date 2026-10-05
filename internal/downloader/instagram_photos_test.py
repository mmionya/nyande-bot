"""Run with: python3 internal/downloader/instagram_photos_test.py"""

from yt_dlp import YoutubeDL

from instagram_photos import InstagramPhotosIE


with YoutubeDL({
    "quiet": True,
    "format": "bestvideo[height<=1080]+bestaudio/best[height<=1080]/best",
}) as ydl:
    extractor = InstagramPhotosIE(ydl)
    product = {
        "pk": "1000000000000000001",
        "media_type": 1,
        "image_versions2": {"candidates": [
            {"url": "https://example.org/large.jpg", "width": 1440, "height": 1800},
            {"url": "https://example.org/small.jpg", "width": 240, "height": 300},
            {"url": "https://example.org/invalid.mp4", "width": 2000, "height": 2000},
        ]},
    }
    info = extractor._extract_product_media(product)
    assert len(info["formats"]) == 1
    assert info["formats"][0]["url"] == "https://example.org/large.jpg"
    selected = ydl.process_ie_result({"title": "photo", **info}, download=False)
    assert selected["url"] == "https://example.org/large.jpg"
    assert selected["ext"] == "jpg"

    # GraphQL currently omits candidate dimensions and sends the original first.
    original = extractor._extract_product_media({
        **product,
        "original_width": 1440,
        "original_height": 1800,
        "image_versions2": {"candidates": [
            {"url": "https://example.org/original.webp"},
            {"url": "https://example.org/resized.webp?stp=dst-webp_s150x150"},
        ]},
    })
    assert original["formats"][0]["url"] == "https://example.org/original.webp"
    assert original["formats"][0]["ext"] == "webp"
    tied = extractor._extract_product_media({
        **product,
        "image_versions2": {"candidates": [
            {"url": "https://example.org/first.jpg", "width": 1080, "height": 1080},
            {"url": "https://example.org/second.jpg", "width": 1080, "height": 1080},
        ]},
    })
    assert tied["formats"][0]["url"] == "https://example.org/first.jpg"

    for media_type in (2, None):
        assert not extractor._extract_product_media({
            **product, "media_type": media_type,
        })["formats"], "A video cover or unknown media must not become a photo"

    video_product = {
        **product,
        "media_type": 2,
        "video_versions": [{"url": "https://example.org/video.mp4", "type": 101}],
    }
    video = extractor._extract_product_media(video_product)
    assert [item["url"] for item in video["formats"]] == ["https://example.org/video.mp4"]
    carousel = extractor._extract_product({"carousel_media": [
        product, video_product, {
            **product,
            "image_versions2": {"candidates": [{"url": "https://example.org/last.jpg"}]},
        },
    ]}, get_comments=False)
    assert [entry["formats"][0]["url"] for entry in carousel["entries"]] == [
        "https://example.org/large.jpg", "https://example.org/video.mp4", "https://example.org/last.jpg",
    ]
    for candidates in ([], [{"url": "https://example.org/invalid.mp4"}], [{"url": "invalid"}]):
        assert not extractor._extract_product_media({
            **product, "image_versions2": {"candidates": candidates},
        })["formats"]
