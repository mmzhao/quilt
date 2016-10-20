#!/bin/bash
set -eo pipefail

timestamp() {
    until ping -q -c1 localhost > /dev/null 2>&1; do
        sleep 0.5
    done
    date -u +%s > /tmp/boot_timestamp
}
timestamp &

# Master: Pass in the flag, and the server-id
# Slave: Pass in the flag, master hostname, and the server-id

DI_MYSQL_MASTER=false
DI_MYSQL_SLAVE=false
DI_MYSQL_ID=0
DI_CONFIG="/etc/mysql/conf.d/di.cnf"
if [ "$1" = "--master" ]; then
    DI_MYSQL_MASTER=true
    DI_MYSQL_ID=$2
    shift 2
fi
if [ "$1" = "--replica" ]; then
    DI_MYSQL_SLAVE=true
    DI_MASTER_HOST=$2
    DI_MYSQL_ID=$3
    shift 3
fi

if [ "$DI_MYSQL_MASTER" = true ]; then
    sed -i "s/\(\[mysqld\]\)/\1\nserver-id = $DI_MYSQL_ID/" /etc/mysql/my.cnf
    sed -i "s|\(\[mysqld\]\)|\1\nlog_bin = /var/log/mysql/mysql-bin.log|" /etc/mysql/my.cnf
fi
if [ "$DI_MYSQL_SLAVE" = true ]; then
    sed -i "s/\(\[mysqld\]\)/\1\nserver-id = $DI_MYSQL_ID/" /etc/mysql/my.cnf
fi

di_master_infodump() {
    # Wait until main process is running
    until mysql "-p$MYSQL_ROOT_PASSWORD" -ve "SHOW MASTER STATUS;" > /dev/null 2>&1; do
        sleep 1
    done

    mysql "-p$MYSQL_ROOT_PASSWORD" -ve "SHOW MASTER STATUS;" | grep 'mysql-bin' | sed "s/\(mysql-bin\.[0-9]\+\)\s\+\([0-9]\+\).*/\1;\2/" > /etc/mysql/masterinfo

    # Serve file over dropbear ssh server
    service dropbear start
}

initialize_di() {
    if [ "$DI_MYSQL_MASTER" = true ]; then
        mysql "-p$MYSQL_ROOT_PASSWORD" -ve "
            CREATE USER 'slave'@'%' IDENTIFIED BY 'slave';
            GRANT REPLICATION SLAVE ON *.* TO 'slave'@'%';"
    fi
    if [ "$DI_MYSQL_SLAVE" = true ]; then
        echo "trying to reach $DI_MASTER_HOST"
        until ping -q -c1 "$DI_MASTER_HOST" > /dev/null 2>&1; do
            sleep 1
        done
        echo "successfully reached $DI_MASTER_HOST"

        # Wait until ssh can connect
        echo "Attempting to reach $DI_MASTER_HOST through ssh"
        until ssh -o BatchMode=yes -o ConnectTimeout=5 -o "StrictHostKeyChecking=no" -i "/root/.ssh/mysql.key" "$DI_MASTER_HOST" exit > /dev/null 2>&1; do
            continue
        done
        echo "Reached $DI_MASTER_HOST through ssh"
        DI_MASTER_INFO=$(ssh -o "StrictHostKeyChecking=no" -i "/root/.ssh/mysql.key" "$DI_MASTER_HOST" "cat /etc/mysql/masterinfo")
        DI_MASTER_LOG=$(echo $DI_MASTER_INFO | sed "s/;.\+$//")
        DI_MASTER_POSITION=$(echo $DI_MASTER_INFO | sed "s/^.\+;//")

        mysql "-p$MYSQL_ROOT_PASSWORD" -ve "
            CHANGE MASTER TO
            MASTER_HOST='$DI_MASTER_HOST',
            MASTER_USER='slave',
            MASTER_PASSWORD='slave',
            MASTER_LOG_FILE='$DI_MASTER_LOG',
            MASTER_LOG_POS=$DI_MASTER_POSITION;"

        mysql "-p$MYSQL_ROOT_PASSWORD" -ve "START SLAVE;"
    fi
}

# if command starts with an option, prepend mysqld
if [ "${1:0:1}" = '-' ]; then
	set -- mysqld "$@"
fi

# skip setup if they want an option that stops mysqld
wantHelp=
for arg; do
	case "$arg" in
		-'?'|--help|--print-defaults|-V|--version)
			wantHelp=1
			break
			;;
	esac
done

if [ "$1" = 'mysqld' -a -z "$wantHelp" ]; then
	# Get config
	DATADIR="$("$@" --verbose --help 2>/dev/null | awk '$1 == "datadir" { print $2; exit }')"

	if [ ! -d "$DATADIR/mysql" ]; then
		if [ -z "$MYSQL_ROOT_PASSWORD" -a -z "$MYSQL_ALLOW_EMPTY_PASSWORD" -a -z "$MYSQL_RANDOM_ROOT_PASSWORD" ]; then
			echo >&2 'error: database is uninitialized and password option is not specified '
			echo >&2 '  You need to specify one of MYSQL_ROOT_PASSWORD, MYSQL_ALLOW_EMPTY_PASSWORD and MYSQL_RANDOM_ROOT_PASSWORD'
			exit 1
		fi

		mkdir -p "$DATADIR"
		chown -R mysql:mysql "$DATADIR"

		echo 'Initializing database'
		"$@" --initialize-insecure
		echo 'Database initialized'

		"$@" --skip-networking &
		pid="$!"

		mysql=( mysql --protocol=socket -uroot )

		for i in {30..0}; do
			if echo 'SELECT 1' | "${mysql[@]}" &> /dev/null; then
				break
			fi
			echo 'MySQL init process in progress...'
			sleep 1
		done
		if [ "$i" = 0 ]; then
			echo >&2 'MySQL init process failed.'
			exit 1
		fi

		if [ -z "$MYSQL_INITDB_SKIP_TZINFO" ]; then
			# sed is for https://bugs.mysql.com/bug.php?id=20545
			mysql_tzinfo_to_sql /usr/share/zoneinfo | sed 's/Local time zone must be set--see zic manual page/FCTY/' | "${mysql[@]}" mysql
		fi

		if [ ! -z "$MYSQL_RANDOM_ROOT_PASSWORD" ]; then
			MYSQL_ROOT_PASSWORD="$(pwgen -1 32)"
			echo "GENERATED ROOT PASSWORD: $MYSQL_ROOT_PASSWORD"
		fi
		"${mysql[@]}" <<-EOSQL
			-- What's done in this file shouldn't be replicated
			--  or products like mysql-fabric won't work
			SET @@SESSION.SQL_LOG_BIN=0;

			DELETE FROM mysql.user ;
			CREATE USER 'root'@'%' IDENTIFIED BY '${MYSQL_ROOT_PASSWORD}' ;
			GRANT ALL ON *.* TO 'root'@'%' WITH GRANT OPTION ;
			DROP DATABASE IF EXISTS test ;
			FLUSH PRIVILEGES ;
		EOSQL

		if [ ! -z "$MYSQL_ROOT_PASSWORD" ]; then
			mysql+=( -p"${MYSQL_ROOT_PASSWORD}" )
		fi

		if [ "$MYSQL_DATABASE" ]; then
			echo "CREATE DATABASE IF NOT EXISTS \`$MYSQL_DATABASE\` ;" | "${mysql[@]}"
			mysql+=( "$MYSQL_DATABASE" )
		fi

		if [ "$MYSQL_USER" -a "$MYSQL_PASSWORD" ]; then
			echo "CREATE USER '$MYSQL_USER'@'%' IDENTIFIED BY '$MYSQL_PASSWORD' ;" | "${mysql[@]}"

			if [ "$MYSQL_DATABASE" ]; then
				echo "GRANT ALL ON \`$MYSQL_DATABASE\`.* TO '$MYSQL_USER'@'%' ;" | "${mysql[@]}"
			fi

			echo 'FLUSH PRIVILEGES ;' | "${mysql[@]}"
		fi

		initialize_di

		echo
		for f in /docker-entrypoint-initdb.d/*; do
			case "$f" in
				*.sh)     echo "$0: running $f"; . "$f" ;;
				*.sql)    echo "$0: running $f"; "${mysql[@]}" < "$f"; echo ;;
				*.sql.gz) echo "$0: running $f"; gunzip -c "$f" | "${mysql[@]}"; echo ;;
				*)        echo "$0: ignoring $f" ;;
			esac
			echo
		done

		if [ ! -z "$MYSQL_ONETIME_PASSWORD" ]; then
			"${mysql[@]}" <<-EOSQL
				ALTER USER 'root'@'%' PASSWORD EXPIRE;
			EOSQL
		fi
		if ! kill -s TERM "$pid" || ! wait "$pid"; then
			echo >&2 'MySQL init process failed.'
			exit 1
		fi

		echo
		echo 'MySQL init process done. Ready for start up.'
		echo
	fi

	chown -R mysql:mysql "$DATADIR"
fi

if [ "$DI_MYSQL_MASTER" = true ]; then
    di_master_infodump &
fi

exec "$@"
