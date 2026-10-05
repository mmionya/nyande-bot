from yt_dlp.extractor.instagram import InstagramIE
from yt_dlp.utils import determine_ext, int_or_none


class InstagramPhotosIE(InstagramIE, plugin_name="nyande"):
    def _extract_product_media(self, product_media):
        info = super()._extract_product_media(product_media)
        if product_media.get("media_type") == 1:
            # ponytail: Missing dimensions rely on Instagram's original-first order;
            # revisit selection if the API stops ordering candidates by quality.
            photos = [
                photo for photo in reversed(info["thumbnails"])
                if determine_ext(photo["url"], "jpg") in ("jpg", "jpeg", "png", "webp")
            ]
            if photos:
                photo = max(photos, key=lambda item: (
                    (int_or_none(item.get("width")) or 0)
                    * (int_or_none(item.get("height")) or 0)
                ))
                info["formats"] = [{
                    **photo,
                    "format_id": "photo",
                    "ext": determine_ext(photo["url"], "jpg"),
                    "acodec": "none",
                }]
        return info
