From wordpress:4.4.2

# Required:
# DB_MASTER: If it's a master-slave setup, then this is the master

# Optional:
# DB_NAME: DB table name, defaults to "wordpress"
# DB_USER: DB username, defaults to "wordpress"
# DB_PASSWORD: DB password, defaults to "wordpress"
# REDIS: A hostname
# MEMCACHED: Comma separated list of hostnames
# DB_SLAVES: Comma separated list of hostnames

RUN apt-get update && \
DEBIAN_FRONTEND="noninteractive" apt-get install -y \
    sudo \
    php5-redis \
    unzip \
    && \
rm -rf /var/lib/apt/lists/* && \
curl -o /usr/local/bin/wp-cli https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar && \
chmod +x /usr/local/bin/wp-cli && \
curl -o /usr/src/redis-cache.zip https://downloads.wordpress.org/plugin/redis-cache.zip && \
curl -o /usr/src/memcached.zip https://downloads.wordpress.org/plugin/memcached.zip && \
curl -o /usr/src/hyperdb.zip https://downloads.wordpress.org/plugin/hyperdb.zip && \
pecl install memcache

# XXX This is only here until the official hyperdb plugin works properly
COPY hyperdb /usr/src/hyperdb

# Lifted from the official wordpress
COPY docker-entrypoint.sh /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
CMD ["apache2-foreground"]
