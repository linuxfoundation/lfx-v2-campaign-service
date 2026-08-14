-- Dropping the column DISCARDS every stored pixel id, and they cannot be recovered from
-- anywhere else in this service -- each one was entered by an operator who read it out of
-- Reddit Ads. A re-migration leaves every Reddit connection unable to create a campaign
-- until each pixel is entered again.
ALTER TABLE reddit_ads_connections DROP COLUMN conversion_pixel_id;
