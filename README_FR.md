# nbdproxy

[English](README.md) | [Русский](README_RU.md) | [简体中文](README_CN.md) | [Español](README_ES.md) | Français

Un petit proxy NBD sans dépendances externes qui maintient **le socket du client local ouvert** pendant la reconnexion à un serveur redémarré. TCP et les sockets Unix sont pris en charge des deux côtés. Le `nbd-client` de Linux ne nécessite ni `-persist` ni la reconnexion par netlink, y compris dans des versions comme la 3.26.1.

```text
Linux /dev/nbd0 ── stable local socket ── nbdproxy ── reconnect ── NBD server
```

Conçu pour un serveur local tel que s3nbd, redémarré indépendamment du proxy. Chaque processus expose une exportation configurée à **une seule connexion cliente**. Les requêtes s'exécutent dans leur ordre de réception, avec au plus une requête en attente côté serveur. Ce choix sacrifie le débit pour simplifier l'ordre de réexécution.

## Compilation et démarrage

Nécessite Go 1.24 ou ultérieur ; aucun module externe ni bibliothèque C.

```sh
go build -trimpath -o nbdproxy .
./nbdproxy -version
sudo install -d -m 0755 /run/nbdproxy
sudo ./nbdproxy \
  -listen unix:/run/nbdproxy/nbd.sock \
  -upstream tcp:127.0.0.1:10809 \
  -export disk1
```

Dans un autre terminal sous Linux :

```sh
sudo modprobe nbd
sudo nbd-client -unix /run/nbdproxy/nbd.sock /dev/nbd0 -N disk1 -b 4096
```

Utilisez **une seule connexion**, sans `-C 2`. `-persist` est inutile pour les redémarrages du serveur. Évitez un délai d'expiration fini pour les E/S NBD du noyau si les interruptions peuvent le dépasser : l'expiration d'une requête du noyau peut faire échouer les E/S même si le socket local reste ouvert. Pour un périphérique existant, consultez `/sys/block/nbd0/queue/io_timeout` (en millisecondes, si le noyau l'expose). Une valeur nulle de `nbd-client -timeout` conserve le réglage actuel ou par défaut du noyau ; elle ne permet pas de réinitialiser de manière fiable un délai précédemment défini sur un périphérique réutilisé. Vérifiez le réglage effectif du noyau.

Exemple avec une interface cliente TCP et un serveur Unix :

```sh
./nbdproxy -listen tcp:127.0.0.1:10810 -upstream unix:/run/s3nbd/nbd.sock -export disk1
sudo nbd-client 127.0.0.1 10810 /dev/nbd0 -N disk1 -b 4096
```

Aucun des deux côtés n'utilise d'authentification ni de TLS. Limitez l'écoute TCP à l'interface de bouclage ou à un réseau de confiance restreint. Le socket Unix côté client est créé avec les permissions 0600. Son répertoire parent doit déjà exister. Les chemins de sockets existants ne sont pas supprimés automatiquement au démarrage ; vérifiez et supprimez manuellement les sockets résiduels si nécessaire. Go supprime le socket de l'écouteur Unix lors d'une fermeture normale.

## Ce qui survit à un redémarrage

1. Le proxy négocie l'exportation configurée avec le serveur avant de terminer la négociation du client. L'absence du serveur lors de la connexion initiale retarde la négociation : la première connexion de `nbd-client` peut encore expirer et nécessiter une nouvelle tentative.
2. Pendant la transmission, chaque requête et ses données WRITE restent en mémoire jusqu'à réception d'une réponse complète du serveur. Une réponse READ partielle est abandonnée.
3. En cas d'erreur de connexion, de négociation, de transport ou d'expiration des E/S du serveur, le proxy attend et se reconnecte indéfiniment. Le socket du client local reste ouvert. Les réponses d'erreur NBD du serveur sont transmises normalement, sans nouvelle tentative.
4. Lors de la reconnexion, la taille et les indicateurs de transmission du serveur doivent être strictement identiques aux valeurs initiales. Une différence termine la session cliente pour éviter de rejouer sur une exportation incompatible. L'adresse et le nom configurés doivent toujours désigner **le même stockage** : NBD n'impose pas d'UUID d'exportation persistant, donc les contrôles de taille, d'indicateurs et de nom ne détectent pas le remplacement par un autre disque de même taille.
5. La requête en attente est rejouée avant l'envoi de toute requête ultérieure.

La réexécution garantit **au moins une fois**, et non exactement une fois. Une WRITE peut avoir abouti avant la perte de sa réponse. La répéter n'est sûr que si l'ancienne opération du serveur ne peut plus s'exécuter ultérieurement, après des opérations plus récentes. Un processus serveur arrêté satisfait cette condition ; les partitions réseau arbitraires, les délais expirés face à un serveur actif ou bloqué et les écritures concurrentes nécessitent des garanties d'isolement (fencing) ou de sérialisation côté serveur. Ne l'utilisez pas comme système générique de basculement distribué ni avec un autre processus écrivant sur le même stockage.

Il n'y a ni journal ni cache d'écriture persistant. Un serveur qui confirme des écritures volatiles peut les perdre lors d'un plantage ; le proxy ne peut pas récupérer les requêtes qu'il a déjà confirmées au noyau. La durabilité FLUSH/FUA du serveur reste essentielle. La réexécution exige aussi que les implémentations WRITE/TRIM du serveur puissent être répétées sans risque. Redémarrer ou tuer **le proxy lui-même** coupe la connexion cliente et n'est pas couvert par la reconnexion au serveur.

## Protocole et limites de ressources

- NBD fixed-newstyle ; EXPORT_NAME, INFO, GO, LIST et ABORT côté client.
- READ, WRITE, FLUSH, TRIM et FUA négocié ; réponses simples uniquement.
- La négociation avec le serveur utilise EXPORT_NAME avec le nom configuré. Le serveur doit accepter ce mode et ses contraintes implicites de taille de bloc (minimum 1 octet).
- Les options non prises en charge (dont les réponses structurées et TLS) reçoivent ERR_UNSUP ; les commandes ou indicateurs non pris en charge reçoivent EINVAL. Les exportations en lecture seule refusent les écritures.
- La capacité de connexions multiples est masquée même si le serveur l'annonce. Une deuxième connexion cliente est fermée sans négociation.
- Limite READ/WRITE : 32 Mio par requête. Une requête en cours plus une requête lue à l'avance ; les tampons TCP/socket assurent la contre-pression. Les tampons de données de l'application sont limités à environ 96 Mio par session active (réponse de lecture comprise), quelle que soit la durée de l'interruption. Les plages TRIM ne transportent pas de données.
- Une signature invalide, des options ou données WRITE trop volumineuses, ou une déconnexion cliente terminent la session. Un client qui ne termine jamais l'envoi d'une requête peut occuper l'unique session ; il s'agit d'un service local de confiance, pas d'un point d'accès public multiutilisateur.
- Les erreurs du serveur sont consignées sur stderr avec le journaliseur structuré de Go. Une négociation ou une réponse serveur constamment invalide entraîne de nouvelles tentatives ; consultez les journaux pour le diagnostic.

Exécutez `./nbdproxy -help` pour les options :

| Option | Valeur par défaut | Signification |
|---|---|---|
| `-listen` | `unix:/run/nbdproxy/nbd.sock` | Adresse d'écoute locale |
| `-upstream` | `tcp:127.0.0.1:10809` | Adresse du serveur |
| `-export` | vide | Nom d'exportation des deux côtés |
| `-retry` | `1s` | Délai fixe entre les tentatives de reconnexion |
| `-connect-timeout` | `5s` | Délai de connexion au serveur |
| `-handshake-timeout` | `30s` | Délai de négociation par connexion |
| `-io-timeout` | `1m` | Délai de requête au serveur et d'écriture de la réponse au client |

L'expiration d'une requête au serveur déclenche une reconnexion ; elle ne limite pas la durée totale de récupération. Le proxy ne fixe pas de délai de lecture côté client pendant la transmission, les clients inactifs restent donc connectés. SIGINT/SIGTERM ferme l'écouteur et la session active, annule les nouvelles tentatives et termine le processus. Démontez les systèmes de fichiers et déconnectez NBD avant d'arrêter le proxy.

## systemd

Installez le binaire, modifiez upstream/export dans l'unité fournie, puis activez-la :

```sh
sudo install -m 0755 nbdproxy /usr/local/bin/nbdproxy
sudo install -m 0644 packaging/nbdproxy.service /etc/systemd/system/nbdproxy.service
sudo systemctl daemon-reload
sudo systemctl enable --now nbdproxy
sudo journalctl -u nbdproxy -f
```

L'unité utilise DynamicUser, crée `/run/nbdproxy` et ne nécessite pas les privilèges root. Root peut connecter `nbd-client` à son socket aux permissions 0600. Un serveur Unix doit autoriser l'accès à l'utilisateur du service ; pour ce déploiement, remplacez DynamicUser par un utilisateur/groupe fixe et configurez les permissions du socket. Gardez cette unité indépendante du service serveur : n'ajoutez pas de PartOf/BindsTo qui arrêterait le proxy au redémarrage du serveur. Restart=on-failure redémarre le processus mais ne conserve pas les sessions NBD existantes.

## Validation

```sh
go vet ./...
go test -race ./...
go test -run='^$' -fuzz=FuzzExportName -fuzztime=10s
```

Les tests couvrent les écritures terminées dont la réponse est perdue, les réponses de lecture partielles, l'ordre des flush, les changements d'exportation, l'annulation, la négociation malformée, la validation des requêtes et le fuzzing de l'analyseur. La CI exécute les tests sous Linux et macOS et compile toutes les cibles de publication. La tâche Linux connecte aussi un vrai `/dev/nbd0` avec le nbd-client de la distribution, effectue des E/S directes, arrête le serveur, vérifie qu'une E/S reste en attente, redémarre le serveur et vérifie les données ainsi que les écritures/flush suivants.

Pour lancer manuellement ce test d'intégration qui écrase le périphérique, utilisez **un hôte Linux jetable avec un `/dev/nbd0` inutilisé** et installez nbd-client :

```sh
sudo modprobe nbd nbds_max=4 max_part=0
go test -c -o /tmp/nbdproxy.test
sudo env NBDPROXY_KERNEL_TEST=1 /tmp/nbdproxy.test \
  -test.v -test.run '^TestLinuxKernelRestart$' -test.timeout 60s
```

Le test écrit sur `/dev/nbd0`. Il doit être activé explicitement et est ignoré lors des tests ordinaires. Le test avec le vrai noyau ne peut pas s'exécuter sous macOS.

## Publications

```sh
go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.41.0
./scripts/release.sh v0.1.0
```

Produit dans `dist/` des archives pour Linux amd64/arm64 et macOS amd64/arm64, chacune contenant le binaire autonome, la documentation, la licence et l'unité systemd, des paquets DEB et RPM pour Linux amd64/arm64 et des sommes de contrôle SHA-256 de tous les artefacts.

Après avoir envoyé ce dépôt sur GitHub, l'envoi d'un tag `v*` déclenche le workflow de publication. Il exécute vet et les tests, construit les archives et les paquets et publie immédiatement une GitHub Release avec des notes générées automatiquement. Les workflows de CI et de publication n'ont que des permissions de lecture, sauf contents:write pour la tâche de publication.

Référence du protocole : [spécification NBD](https://github.com/NetworkBlockDevice/nbd/blob/master/doc/proto.md).
Licence : MIT.
