From mysql:5.7.11

# XXX The SSH keys used for communication between Mysql masters and slaves
# is insecure because it is embedded inside the container. This should be
# fixed.

RUN apt-get update && \
DEBIAN_FRONTEND="noninteractive" apt-get install -y dropbear openssh-client && \
rm -rf /var/lib/apt/lists/* && \
ssh-keygen -t rsa -N "" -f /root/.ssh/mysql.key && \
cat /root/.ssh/mysql.key.pub >> /root/.ssh/authorized_keys && \
chmod -R 600 /root/.ssh

ENV MYSQL_ROOT_PASSWORD=mysql \
MYSQL_DATABASE=wordpress \
MYSQL_USER=wordpress \
MYSQL_PASSWORD=wordpress

# Lifted from the official mysql
COPY docker-entrypoint.sh /usr/local/bin/
RUN ln -sf usr/local/bin/docker-entrypoint.sh /entrypoint.sh # backwards compat
ENTRYPOINT ["docker-entrypoint.sh"]

CMD ["mysqld"]
