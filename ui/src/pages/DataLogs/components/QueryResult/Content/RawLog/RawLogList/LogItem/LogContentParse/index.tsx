import classNames from "classnames";
import logItemStyles from "@/pages/DataLogs/components/QueryResult/Content/RawLog/RawLogList/LogItem/index.less";
import { useModel } from "@@/plugin-model/useModel";
import JsonView from "@/components/JsonView";
import JsonStringValue from "@/components/JsonView/JsonStringValue";

type LogContentParseProps = {
  logContent: any;
  secondaryIndexKeys?: any[];
  keyItem?: string;
  quickInsertLikeQuery: (key: string) => void;
};

const LogContentParse = ({
  logContent,
  keyItem,
  secondaryIndexKeys,
  quickInsertLikeQuery,
}: LogContentParseProps) => {
  const { highlightKeywords } = useModel("dataLogs");

  const isNullList = ["\n", "\r\n", "", " "];

  let content;
  
  if (typeof logContent !== "object") {
    if (isNullList.includes(logContent)) {
      content = "";
    } else {
      content = (
        <JsonStringValue
          val={logContent.toString()}
          keyItem={keyItem}
          onClickValue={quickInsertLikeQuery}
          highLightValue={highlightKeywords}
        />
      );
    }
  } else if (logContent === null) {
    content = "";
  } else {
    content = (
      <>
        <JsonView
          secondaryIndexKeys={secondaryIndexKeys}
          data={logContent}
          onClickValue={quickInsertLikeQuery}
          highLightValue={highlightKeywords}
        />
      </>
    );
  }
  return (
    <span className={classNames(logItemStyles.logContent)}>
      {/* {JSON.stringify(content)} */}
      {content}
    </span>
  );
};
export default LogContentParse;
