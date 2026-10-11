MODEL=gemma4:e4b
DIR=results/gemma4-e4b
EPOCHS=5
mkdir -p $DIR

for i in $(seq 1 $EPOCHS); do
  jq --arg m "$MODEL" '.model = $m' req.json \
  | curl -s http://localhost:11434/api/chat -d @- > $DIR/run-$i.json
  jq -e '.message.content | fromjson | .coupling | IN("low","medium","high")' $DIR/run-$i.json >/dev/null \
    && echo pass || echo fail
done | sort | uniq -c | tee $DIR/summary.txt
